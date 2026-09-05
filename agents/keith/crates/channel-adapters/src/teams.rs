#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet, VecDeque};
use std::fmt::Display;
use std::time::Duration;

use chrono::DateTime;
use keith_agent_types::{ProfileId, UtcTimestamp};
use keith_channel_core::{
    AdapterCapability, AdapterEvent, AdapterFailure, AdapterFeatures, Attachment,
    ChannelAccountSetupV2, ChannelAdapter, ChannelAdapterErrorKindV2, ChannelAdapterErrorV2,
    ChannelAdapterV2, ChannelAttachmentKindV2, ChannelAttachmentV2, ChannelCapabilitiesV2,
    ChannelCapabilitySupportV2, ChannelCapabilityV2, ChannelConnectionHealthV2,
    ChannelContractVersion, ChannelConversationKindV2, ChannelConversationV2, ChannelEventKindV2,
    ChannelEventV2, ChannelIdentityV2, ChannelMentionV2, ChannelMessageV2,
    ChannelOperationReceiptV2, ChannelOperationV2, ChannelOutboundMessageV2, ChannelReceiptStateV2,
    ChannelRichContentV2, InboundIntent, InboundMessage, OutboundMessage, ReconnectCursorV2,
    RetryClass, SendReceipt,
};
use keith_credentials::{CredentialOwner, CredentialRef, SecretValue};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

const BOT_FRAMEWORK_ISSUER: &str = "https://api.botframework.com";
const DEFAULT_MAX_EVENT_BYTES: usize = 1024 * 1024;
const DEFAULT_MAX_ATTACHMENT_BYTES: u64 = 25 * 1024 * 1024;
const DEFAULT_DEDUPLICATION_CAPACITY: usize = 4096;

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TeamsConfig {
    pub bot_app_id: String,
    pub external_account: String,
    pub profile_id: ProfileId,
    pub credential_ref: CredentialRef,
    pub max_event_bytes: usize,
    pub max_attachment_bytes: u64,
    pub timeout_ms: u64,
    pub deduplication_capacity: usize,
}

impl TeamsConfig {
    pub fn production(
        bot_app_id: impl Into<String>,
        external_account: impl Into<String>,
        profile_id: ProfileId,
        credential_ref: CredentialRef,
    ) -> Self {
        Self {
            bot_app_id: bot_app_id.into(),
            external_account: external_account.into(),
            profile_id,
            credential_ref,
            max_event_bytes: DEFAULT_MAX_EVENT_BYTES,
            max_attachment_bytes: DEFAULT_MAX_ATTACHMENT_BYTES,
            timeout_ms: 30_000,
            deduplication_capacity: DEFAULT_DEDUPLICATION_CAPACITY,
        }
    }
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeamsCursor {
    pub recent_activity_ids: Vec<String>,
    pub conversation_service_urls: BTreeMap<String, String>,
    #[serde(default)]
    pub last_activity_at: Option<UtcTimestamp>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TeamsVerifiedClaims {
    pub issuer: String,
    pub audience: String,
    pub key_id: String,
    pub service_url: String,
    pub expires_at: UtcTimestamp,
}

/// Cryptographic verification remains at the HTTP ingress boundary. Implementations must verify
/// the Bot Connector JWT signature against its discovered JWKS and return its signed service URL.
pub trait TeamsRequestVerifier {
    type Error: Display;

    /// # Errors
    ///
    /// Returns an error when the connector token signature, issuer, audience, expiry, or service
    /// URL cannot be verified.
    fn verify(
        &self,
        authorization: &str,
        expected_audience: &str,
        now: UtcTimestamp,
    ) -> Result<TeamsVerifiedClaims, Self::Error>;
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TeamsIngestOutcome {
    Queued,
    Duplicate,
    Ignored,
}

#[derive(Clone, Debug, Eq, PartialEq)]
#[allow(clippy::struct_excessive_bools)]
pub struct TeamsCapabilities {
    pub messages: bool,
    pub replies: bool,
    pub threads: bool,
    pub mentions: bool,
    pub inbound_files: bool,
    pub outbound_files: bool,
    pub inbound_cards: bool,
    pub outbound_cards: bool,
    pub reconnect: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TeamsSetupDiagnostics {
    pub profile_id: ProfileId,
    pub external_account: String,
    pub credential_ref: CredentialRef,
    pub callback_verified: bool,
    pub revoked: bool,
    pub cursor_activity_count: usize,
    pub capabilities: TeamsCapabilities,
    pub safe_error: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PreparedHttpRequest {
    pub method: String,
    pub url: String,
    pub content_type: String,
    pub body: Vec<u8>,
    pub idempotency_key: Option<String>,
}

pub struct TeamsAdapter {
    config: TeamsConfig,
    access_token: SecretValue,
    http: ureq::Agent,
    cursor: TeamsCursor,
    seen: BTreeSet<String>,
    seen_order: VecDeque<String>,
    inbound: VecDeque<ChannelEventV2>,
    cancelled: BTreeSet<String>,
    callback_verified: bool,
    revoked: bool,
    safe_error: Option<String>,
}

impl TeamsAdapter {
    /// Creates an adapter whose bearer credential is scoped to this Teams account.
    ///
    /// # Errors
    ///
    /// Returns a permanent failure for invalid bounds or a credential owned by another channel.
    pub fn new(
        config: TeamsConfig,
        access_token: SecretValue,
        cursor: TeamsCursor,
    ) -> Result<Self, AdapterFailure> {
        if !valid_channel_identity(&config.bot_app_id)
            || !valid_channel_identity(&config.external_account)
            || config.max_event_bytes == 0
            || config.max_attachment_bytes == 0
            || config.timeout_ms == 0
            || config.deduplication_capacity == 0
            || !credential_belongs_to(&config.credential_ref, &config.external_account)
            || cursor
                .conversation_service_urls
                .values()
                .any(|url| !allowed_teams_service_url(url))
        {
            return Err(permanent("invalid Teams adapter configuration"));
        }
        let http: ureq::Agent = ureq::Agent::config_builder()
            .timeout_global(Some(Duration::from_millis(config.timeout_ms)))
            .http_status_as_error(false)
            .build()
            .into();
        let seen_order = bounded_recent(&cursor.recent_activity_ids, config.deduplication_capacity);
        let seen = seen_order.iter().cloned().collect();
        Ok(Self {
            config,
            access_token,
            http,
            cursor,
            seen,
            seen_order,
            inbound: VecDeque::new(),
            cancelled: BTreeSet::new(),
            callback_verified: false,
            revoked: false,
            safe_error: None,
        })
    }

    pub const fn cursor(&self) -> &TeamsCursor {
        &self.cursor
    }

    #[allow(clippy::unused_self)]
    pub fn capabilities(&self) -> TeamsCapabilities {
        TeamsCapabilities {
            messages: true,
            replies: true,
            threads: true,
            mentions: true,
            inbound_files: true,
            outbound_files: false,
            inbound_cards: true,
            outbound_cards: false,
            reconnect: true,
        }
    }

    pub fn setup_diagnostics(&self) -> TeamsSetupDiagnostics {
        TeamsSetupDiagnostics {
            profile_id: self.config.profile_id.clone(),
            external_account: self.config.external_account.clone(),
            credential_ref: self.config.credential_ref.clone(),
            callback_verified: self.callback_verified,
            revoked: self.revoked,
            cursor_activity_count: self.cursor.recent_activity_ids.len(),
            capabilities: self.capabilities(),
            safe_error: self.safe_error.clone(),
        }
    }

    pub fn account_setup_v2(&self) -> ChannelAccountSetupV2 {
        ChannelAccountSetupV2 {
            account_id: self.config.external_account.clone(),
            required_credential_names: BTreeSet::from([
                "bot_framework_access_token".to_owned(),
                "bot_framework_request_verifier".to_owned(),
            ]),
            required_scopes: BTreeSet::from(["https://api.botframework.com/.default".to_owned()]),
            webhook_configured: true,
            socket_or_polling_configured: false,
            connection_health: if self.revoked {
                ChannelConnectionHealthV2::Revoked
            } else if self.safe_error.is_some() {
                ChannelConnectionHealthV2::Failed
            } else if self.callback_verified {
                ChannelConnectionHealthV2::Connected
            } else {
                ChannelConnectionHealthV2::Disconnected
            },
            reconnect_cursor_present: !self.cursor.recent_activity_ids.is_empty(),
            safe_test_supported: !self.cursor.conversation_service_urls.is_empty(),
            metadata: BTreeMap::from([
                ("bot_app_id".to_owned(), self.config.bot_app_id.clone()),
                (
                    "callback_verified".to_owned(),
                    self.callback_verified.to_string(),
                ),
                (
                    "known_conversations".to_owned(),
                    self.cursor.conversation_service_urls.len().to_string(),
                ),
            ]),
        }
    }

    /// Prepares the read-only Bot Connector members request used to test this account.
    ///
    /// # Errors
    ///
    /// Returns a permanent failure until a verified conversation reference is available.
    pub fn prepare_test_connection(&self) -> Result<PreparedHttpRequest, AdapterFailure> {
        self.ensure_active()?;
        let (conversation, service_url) = self
            .cursor
            .conversation_service_urls
            .iter()
            .next()
            .ok_or_else(|| permanent("Teams safe test needs a verified conversation"))?;
        Ok(PreparedHttpRequest {
            method: "GET".to_owned(),
            url: format!(
                "{}/v3/conversations/{}/members",
                service_url.trim_end_matches('/'),
                percent_encode(conversation)
            ),
            content_type: "application/json".to_owned(),
            body: Vec::new(),
            idempotency_key: None,
        })
    }

    /// Runs a read-only Bot Connector membership lookup using the scoped bot credential.
    ///
    /// # Errors
    ///
    /// Returns a precisely classified authentication, permission, rate-limit, transport, or
    /// destination failure.
    pub fn test_connection(&self) -> Result<(), ChannelAdapterErrorV2> {
        let request = self
            .prepare_test_connection()
            .map_err(ChannelAdapterErrorV2::from)?;
        let response = self.access_token.with_bytes(|token| {
            let token = std::str::from_utf8(token).map_err(|_| {
                v2_error(
                    ChannelAdapterErrorKindV2::Authentication,
                    "Teams access token is not UTF-8",
                )
            })?;
            self.http
                .get(&request.url)
                .header("Authorization", format!("Bearer {token}"))
                .call()
                .map_err(|error| ChannelAdapterErrorV2::from(transport_error("Teams", &error)))
        })?;
        classify_test_status("Teams", response.status().as_u16())
    }

    /// Verifies the authorization token before parsing the untrusted Bot Framework activity.
    ///
    /// # Errors
    ///
    /// Returns a classified failure for revoked accounts, failed JWT verification, malformed
    /// activities, oversized events, or attachments beyond the configured bound.
    pub fn ingest_verified_activity<V: TeamsRequestVerifier>(
        &mut self,
        authorization: &str,
        body: &[u8],
        verifier: &V,
    ) -> Result<TeamsIngestOutcome, AdapterFailure> {
        self.ensure_active()?;
        let now = now();
        let claims = verifier
            .verify(authorization, &self.config.bot_app_id, now)
            .map_err(|error| {
                self.safe_error = Some("Teams request authentication failed".to_owned());
                AdapterFailure {
                    class: RetryClass::Permanent,
                    safe_message: format!("Teams request authentication failed: {error}"),
                    retry_after_ms: None,
                }
            })?;
        if claims.issuer != BOT_FRAMEWORK_ISSUER
            || claims.audience != self.config.bot_app_id
            || claims.key_id.trim().is_empty()
            || claims.expires_at <= now
        {
            self.safe_error = Some("Teams request claims were rejected".to_owned());
            return Err(permanent("Teams request claims were rejected"));
        }
        self.callback_verified = true;
        if body.len() > self.config.max_event_bytes {
            return Err(permanent("Teams activity exceeds the configured limit"));
        }
        let activity: TeamsActivity =
            serde_json::from_slice(body).map_err(|_| permanent("Teams activity is malformed"))?;
        if activity.activity_type != "message" {
            return Ok(TeamsIngestOutcome::Ignored);
        }
        let message_id = required(activity.id, "Teams activity has no ID")?;
        if self.seen.contains(&message_id) {
            return Ok(TeamsIngestOutcome::Duplicate);
        }
        let conversation = required(
            activity.conversation.and_then(|value| value.id),
            "Teams activity has no conversation",
        )?;
        let sender_identity = activity
            .from
            .ok_or_else(|| permanent("Teams activity has no sender"))?;
        let sender = required(sender_identity.id, "Teams activity has no sender")?;
        let sender_name = sender_identity.name;
        let service_url = required(activity.service_url, "Teams activity has no service URL")?;
        if claims.service_url != service_url || !allowed_teams_service_url(&service_url) {
            self.safe_error = Some("Teams activity service URL was rejected".to_owned());
            return Err(permanent("Teams activity service URL was rejected"));
        }
        let normalized =
            normalize_attachments(activity.attachments, self.config.max_attachment_bytes)?;
        let text = activity.text.unwrap_or_default();
        if text.trim().is_empty()
            && normalized.attachments.is_empty()
            && normalized.rich_content.is_empty()
        {
            return Err(permanent("Teams activity has no message content"));
        }
        let occurred_at = activity
            .timestamp
            .as_deref()
            .and_then(parse_timestamp)
            .unwrap_or(now);
        self.cursor
            .conversation_service_urls
            .insert(conversation.clone(), service_url);
        self.remember(message_id.clone(), occurred_at);
        let conversation = ChannelConversationV2 {
            platform_id: conversation,
            kind: ChannelConversationKindV2::Channel,
            thread_id: activity.reply_to_id.clone(),
            reply_to_message_id: activity.reply_to_id,
        };
        let event = ChannelEventV2 {
            contract: ChannelContractVersion::new(2, 0),
            event_id: message_id.clone(),
            delivery_attempt: 1,
            event: ChannelEventKindV2::MessageCreated(ChannelMessageV2 {
                message_id,
                account_id: self.config.external_account.clone(),
                conversation,
                sender: ChannelIdentityV2 {
                    platform_id: sender,
                    display_name: sender_name,
                    is_bot: false,
                },
                text,
                attachments: normalized.attachments,
                rich_content: normalized.rich_content,
                mentions: normalize_mentions(activity.entities),
                occurred_at,
                metadata: BTreeMap::from([("platform".to_owned(), "teams".to_owned())]),
            }),
            metadata: BTreeMap::new(),
        };
        event
            .validate(&self.capabilities_v2())
            .map_err(adapter_failure_from_v2)?;
        self.inbound.push_back(event);
        self.safe_error = None;
        Ok(TeamsIngestOutcome::Queued)
    }

    pub fn revoke(&mut self) {
        self.revoked = true;
        self.inbound.clear();
        self.safe_error = Some("Teams account was revoked".to_owned());
    }

    /// Prepares the exact non-secret Bot Framework HTTP request used by [`Self::send`].
    ///
    /// # Errors
    ///
    /// Returns a permanent failure for a cross-account route, missing verified service URL,
    /// unsupported files, or an unencodable activity.
    pub fn prepare_send(
        &self,
        message: &OutboundMessage,
    ) -> Result<PreparedHttpRequest, AdapterFailure> {
        self.ensure_active()?;
        if message.route.channel != "teams"
            || message.route.external_account != self.config.external_account
        {
            return Err(permanent(
                "Teams delivery route belongs to another adapter account",
            ));
        }
        if !message.artifacts.is_empty() {
            return Err(permanent(
                "Teams outbound files require the later attachment integration",
            ));
        }
        let service_url = self
            .cursor
            .conversation_service_urls
            .get(&message.route.conversation)
            .ok_or_else(|| permanent("Teams conversation has no verified service URL"))?;
        let url = format!(
            "{}/v3/conversations/{}/activities",
            service_url.trim_end_matches('/'),
            percent_encode(&message.route.conversation)
        );
        let mut payload = json!({
            "type": "message",
            "text": message.text,
            "channelData": {"clientActivityId": message.idempotency_key},
        });
        if let Some(reply_to) = &message.route.reply_to_message {
            payload["replyToId"] = Value::String(reply_to.clone());
        }
        Ok(PreparedHttpRequest {
            method: "POST".to_owned(),
            url,
            content_type: "application/json".to_owned(),
            body: serde_json::to_vec(&payload)
                .map_err(|_| permanent("Teams outbound activity could not be encoded"))?,
            idempotency_key: Some(message.idempotency_key.clone()),
        })
    }

    fn ensure_active(&self) -> Result<(), AdapterFailure> {
        if self.revoked {
            Err(permanent("Teams account was revoked"))
        } else {
            Ok(())
        }
    }

    fn remember(&mut self, activity_id: String, occurred_at: UtcTimestamp) {
        self.seen.insert(activity_id.clone());
        self.seen_order.push_back(activity_id);
        while self.seen_order.len() > self.config.deduplication_capacity {
            if let Some(expired) = self.seen_order.pop_front() {
                self.seen.remove(&expired);
            }
        }
        self.cursor.recent_activity_ids = self.seen_order.iter().cloned().collect();
        self.cursor.last_activity_at = Some(occurred_at);
    }

    fn send_activity(&mut self, message: &OutboundMessage) -> Result<SendReceipt, AdapterFailure> {
        let request = self.prepare_send(message)?;
        let mut response = self.access_token.with_bytes(|token| {
            let token = std::str::from_utf8(token)
                .map_err(|_| permanent("Teams access token is not UTF-8"))?;
            self.http
                .post(&request.url)
                .header("Authorization", format!("Bearer {token}"))
                .header("Content-Type", &request.content_type)
                .send(&request.body)
                .map_err(|error| transport_error("Teams", &error))
        })?;
        let status = response.status().as_u16();
        let response_body = response
            .body_mut()
            .with_config()
            .limit(u64::try_from(self.config.max_event_bytes).unwrap_or(u64::MAX))
            .read_to_vec()
            .map_err(|error| transport_error("Teams", &error))?;
        match status {
            200..=299 => {
                let receipt: TeamsSendReceipt = serde_json::from_slice(&response_body)
                    .map_err(|_| permanent("Teams returned a malformed send receipt"))?;
                Ok(SendReceipt {
                    platform_message_id: receipt.id,
                    accepted_at: now(),
                    duplicate_possible: true,
                })
            }
            401 | 403 => Err(permanent("Teams authentication or permission denied")),
            429 => Err(rate_limited("Teams rate limit reached", None)),
            500..=599 => Err(retryable("Teams service is temporarily unavailable")),
            _ => Err(permanent("Teams rejected the outbound activity")),
        }
    }
}

pub(crate) fn valid_channel_identity(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 512
        && value == value.trim()
        && !value.chars().any(char::is_control)
}

impl ChannelAdapterV2 for TeamsAdapter {
    fn capabilities_v2(&self) -> ChannelCapabilitiesV2 {
        teams_capabilities(&self.config)
    }

    fn receive_v2(&mut self) -> Result<ChannelEventV2, ChannelAdapterErrorV2> {
        self.ensure_active().map_err(ChannelAdapterErrorV2::from)?;
        self.inbound.pop_front().ok_or_else(|| {
            v2_error(
                ChannelAdapterErrorKindV2::TransientNetwork,
                "Teams has no pending verified activity",
            )
        })
    }

    fn execute_v2(
        &mut self,
        operation: &ChannelOperationV2,
    ) -> Result<ChannelOperationReceiptV2, ChannelAdapterErrorV2> {
        self.capabilities_v2()
            .require(operation.required_capability())?;
        match operation {
            ChannelOperationV2::SendMessage(message) => {
                if !message.rich_content.is_empty() {
                    return Err(ChannelAdapterErrorV2::unsupported(
                        "Teams outbound cards are not enabled by this adapter",
                    ));
                }
                if self.cancelled.contains(&message.idempotency_key) {
                    return Err(v2_error(
                        ChannelAdapterErrorKindV2::Cancelled,
                        "Teams delivery was cancelled before dispatch",
                    ));
                }
                let receipt = self
                    .send_activity(&OutboundMessage {
                        route: message.route.clone(),
                        idempotency_key: message.idempotency_key.clone(),
                        text: message.text.clone(),
                        artifacts: message.artifacts.clone(),
                    })
                    .map_err(ChannelAdapterErrorV2::from)?;
                Ok(ChannelOperationReceiptV2 {
                    operation_id: message.idempotency_key.clone(),
                    platform_message_id: Some(receipt.platform_message_id),
                    accepted_at: receipt.accepted_at,
                    state: ChannelReceiptStateV2::Accepted,
                    duplicate_possible: receipt.duplicate_possible,
                    metadata: BTreeMap::new(),
                })
            }
            ChannelOperationV2::Cancel { cancellation_id } => {
                if cancellation_id.trim().is_empty() {
                    return Err(ChannelAdapterErrorV2::malformed(
                        "Teams cancellation identity is empty",
                    ));
                }
                self.cancelled.insert(cancellation_id.clone());
                Ok(operation_receipt(
                    cancellation_id.clone(),
                    None,
                    ChannelReceiptStateV2::Cancelled,
                    false,
                ))
            }
            ChannelOperationV2::EditMessage { .. }
            | ChannelOperationV2::DeleteMessage { .. }
            | ChannelOperationV2::AddReaction { .. }
            | ChannelOperationV2::RemoveReaction { .. }
            | ChannelOperationV2::SetTyping { .. } => Err(ChannelAdapterErrorV2::unsupported(
                "Teams operation is not enabled by this adapter",
            )),
        }
    }

    fn reconnect_v2(&mut self) -> Result<(), ChannelAdapterErrorV2> {
        self.ensure_active().map_err(ChannelAdapterErrorV2::from)?;
        self.safe_error = None;
        Ok(())
    }

    fn reconnect_cursor_v2(&self) -> Option<ReconnectCursorV2> {
        self.cursor
            .recent_activity_ids
            .last()
            .zip(self.cursor.last_activity_at)
            .map(|(value, observed_at)| ReconnectCursorV2 {
                value: value.clone(),
                observed_at,
            })
    }
}

impl ChannelAdapter for TeamsAdapter {
    fn features(&self) -> AdapterFeatures {
        AdapterFeatures {
            capabilities: BTreeSet::from([
                AdapterCapability::Attachments,
                AdapterCapability::Threads,
                AdapterCapability::Reconnect,
            ]),
            max_attachment_bytes: self.config.max_attachment_bytes,
            requests_per_minute: None,
        }
    }

    fn receive(&mut self) -> Result<AdapterEvent, AdapterFailure> {
        loop {
            let event = self.receive_v2().map_err(adapter_failure_from_v2)?;
            if let Some(event) = event_v2_to_v1("teams", event) {
                return Ok(event);
            }
        }
    }

    fn send(&mut self, message: &OutboundMessage) -> Result<SendReceipt, AdapterFailure> {
        let receipt = self
            .execute_v2(&ChannelOperationV2::SendMessage(ChannelOutboundMessageV2 {
                route: message.route.clone(),
                idempotency_key: message.idempotency_key.clone(),
                text: message.text.clone(),
                artifacts: message.artifacts.clone(),
                rich_content: Vec::new(),
                metadata: BTreeMap::new(),
            }))
            .map_err(adapter_failure_from_v2)?;
        Ok(SendReceipt {
            platform_message_id: receipt
                .platform_message_id
                .unwrap_or_else(|| receipt.operation_id.clone()),
            accepted_at: receipt.accepted_at,
            duplicate_possible: receipt.duplicate_possible,
        })
    }

    fn reconnect(&mut self) -> Result<(), AdapterFailure> {
        self.reconnect_v2().map_err(adapter_failure_from_v2)
    }
}

#[derive(Debug, Deserialize)]
struct TeamsActivity {
    #[serde(rename = "type")]
    activity_type: String,
    id: Option<String>,
    timestamp: Option<String>,
    #[serde(rename = "serviceUrl")]
    service_url: Option<String>,
    from: Option<TeamsIdentity>,
    conversation: Option<TeamsIdentity>,
    text: Option<String>,
    #[serde(rename = "replyToId")]
    reply_to_id: Option<String>,
    #[serde(default)]
    attachments: Vec<TeamsAttachment>,
    #[serde(default)]
    entities: Vec<TeamsEntity>,
}

#[derive(Debug, Deserialize)]
struct TeamsIdentity {
    id: Option<String>,
    name: Option<String>,
}

#[derive(Debug, Deserialize)]
struct TeamsAttachment {
    #[serde(rename = "contentType")]
    content_type: Option<String>,
    #[serde(rename = "contentUrl")]
    content_url: Option<String>,
    name: Option<String>,
    content: Option<Value>,
}

#[derive(Debug, Deserialize)]
struct TeamsEntity {
    #[serde(rename = "type")]
    entity_type: String,
    mentioned: Option<TeamsIdentity>,
}

#[derive(Debug, Deserialize)]
struct TeamsSendReceipt {
    id: String,
}

struct NormalizedTeamsContent {
    attachments: Vec<ChannelAttachmentV2>,
    rich_content: Vec<ChannelRichContentV2>,
}

fn normalize_attachments(
    values: Vec<TeamsAttachment>,
    max_attachment_bytes: u64,
) -> Result<NormalizedTeamsContent, AdapterFailure> {
    let mut attachments = Vec::new();
    let mut rich_content = Vec::new();
    for (index, value) in values.into_iter().enumerate() {
        let content_bytes = value
            .content
            .as_ref()
            .and_then(|content| serde_json::to_vec(content).ok())
            .map_or(0, |bytes| bytes.len());
        let byte_length = u64::try_from(content_bytes).unwrap_or(u64::MAX);
        if byte_length > max_attachment_bytes {
            return Err(permanent("Teams attachment exceeds the configured limit"));
        }
        let media_type = value
            .content_type
            .unwrap_or_else(|| "application/octet-stream".to_owned());
        if media_type.starts_with("application/vnd.microsoft.card.") {
            rich_content.push(ChannelRichContentV2 {
                kind: media_type,
                text: value
                    .content
                    .map_or_else(String::new, |content| content.to_string()),
                metadata: BTreeMap::new(),
            });
        } else {
            attachments.push(ChannelAttachmentV2 {
                attachment: Attachment {
                    id: format!("teams-attachment-{index}"),
                    file_name: value.name.unwrap_or_else(|| format!("attachment-{index}")),
                    media_type: media_type.clone(),
                    byte_length,
                    artifact_id: None,
                    download_url: value.content_url,
                    staging_file: None,
                    sha256: None,
                },
                kind: attachment_kind(&media_type),
                duration_ms: None,
                metadata: BTreeMap::new(),
            });
        }
    }
    Ok(NormalizedTeamsContent {
        attachments,
        rich_content,
    })
}

fn normalize_mentions(values: Vec<TeamsEntity>) -> Vec<ChannelMentionV2> {
    values
        .into_iter()
        .filter(|entity| entity.entity_type == "mention")
        .filter_map(|entity| entity.mentioned)
        .filter_map(|identity| {
            Some(ChannelMentionV2 {
                identity: ChannelIdentityV2 {
                    platform_id: identity.id.filter(|id| !id.trim().is_empty())?,
                    display_name: identity.name,
                    is_bot: false,
                },
                start: None,
                end: None,
            })
        })
        .collect()
}

fn attachment_kind(media_type: &str) -> ChannelAttachmentKindV2 {
    if media_type.starts_with("image/") {
        ChannelAttachmentKindV2::Image
    } else if media_type.starts_with("audio/") {
        ChannelAttachmentKindV2::Audio
    } else if media_type.starts_with("video/") {
        ChannelAttachmentKindV2::Video
    } else {
        ChannelAttachmentKindV2::File
    }
}

fn teams_capabilities(config: &TeamsConfig) -> ChannelCapabilitiesV2 {
    let unsupported = |reason: &str| ChannelCapabilitySupportV2::Unsupported {
        safe_reason: reason.to_owned(),
    };
    let mut declarations = ChannelCapabilityV2::ALL
        .into_iter()
        .map(|capability| {
            (
                capability,
                unsupported("Teams capability is not enabled by this adapter"),
            )
        })
        .collect::<BTreeMap<_, _>>();
    for capability in [
        ChannelCapabilityV2::InboundMessages,
        ChannelCapabilityV2::OutboundMessages,
        ChannelCapabilityV2::Threads,
        ChannelCapabilityV2::Replies,
        ChannelCapabilityV2::Mentions,
        ChannelCapabilityV2::Attachments,
        ChannelCapabilityV2::RateLimits,
        ChannelCapabilityV2::Reconnect,
        ChannelCapabilityV2::Cancellation,
    ] {
        declarations.insert(capability, ChannelCapabilitySupportV2::Supported);
    }
    declarations.insert(
        ChannelCapabilityV2::Commands,
        unsupported("Teams commands arrive as ordinary Bot Framework activities"),
    );
    declarations.insert(
        ChannelCapabilityV2::RichContent,
        unsupported("Teams cards are accepted inbound but outbound cards are not enabled"),
    );
    declarations.insert(
        ChannelCapabilityV2::MessageEdits,
        unsupported("Teams activity updates are not enabled by this adapter"),
    );
    declarations.insert(
        ChannelCapabilityV2::MessageDeletion,
        unsupported("Teams activity deletion is not enabled by this adapter"),
    );
    declarations.insert(
        ChannelCapabilityV2::Reactions,
        unsupported("Teams Bot Framework reactions are not enabled by this adapter"),
    );
    declarations.insert(
        ChannelCapabilityV2::Voice,
        unsupported("Teams voice calls are outside the message adapter"),
    );
    declarations.insert(
        ChannelCapabilityV2::Typing,
        unsupported("Teams typing activities are not enabled by this adapter"),
    );
    declarations.insert(
        ChannelCapabilityV2::DeliveryReceipts,
        unsupported("Teams Bot Framework does not expose durable delivery receipts here"),
    );
    declarations.insert(
        ChannelCapabilityV2::ReadReceipts,
        unsupported("Teams Bot Framework does not expose per-message read receipts here"),
    );
    declarations.insert(
        ChannelCapabilityV2::IdempotentSend,
        unsupported("Teams Bot Framework can return an uncertain acknowledgement"),
    );
    ChannelCapabilitiesV2 {
        contract: ChannelContractVersion::new(2, 0),
        declarations,
        max_event_bytes: u64::try_from(config.max_event_bytes).unwrap_or(u64::MAX),
        max_attachment_bytes: config.max_attachment_bytes,
        max_attachments: 10,
        max_rich_content_bytes: u64::try_from(config.max_event_bytes).unwrap_or(u64::MAX),
        requests_per_minute: None,
    }
}

pub(crate) fn operation_receipt(
    operation_id: String,
    platform_message_id: Option<&str>,
    state: ChannelReceiptStateV2,
    duplicate_possible: bool,
) -> ChannelOperationReceiptV2 {
    ChannelOperationReceiptV2 {
        operation_id,
        platform_message_id: platform_message_id.map(str::to_owned),
        accepted_at: now(),
        state,
        duplicate_possible,
        metadata: BTreeMap::new(),
    }
}

pub(crate) fn v2_error(kind: ChannelAdapterErrorKindV2, message: &str) -> ChannelAdapterErrorV2 {
    ChannelAdapterErrorV2 {
        kind,
        safe_message: message.to_owned(),
        retry_after_ms: None,
    }
}

pub(crate) fn classify_test_status(
    platform: &str,
    status: u16,
) -> Result<(), ChannelAdapterErrorV2> {
    let kind = match status {
        200..=299 => return Ok(()),
        401 => ChannelAdapterErrorKindV2::Authentication,
        403 => ChannelAdapterErrorKindV2::Permission,
        429 => ChannelAdapterErrorKindV2::RateLimit,
        500..=599 => ChannelAdapterErrorKindV2::TransientNetwork,
        _ => ChannelAdapterErrorKindV2::PermanentDestination,
    };
    Err(v2_error(
        kind,
        &format!("{platform} safe connection test failed"),
    ))
}

pub(crate) fn adapter_failure_from_v2(error: ChannelAdapterErrorV2) -> AdapterFailure {
    let class = match error.kind {
        ChannelAdapterErrorKindV2::RateLimit => RetryClass::RateLimited,
        ChannelAdapterErrorKindV2::TransientNetwork
        | ChannelAdapterErrorKindV2::UncertainAcknowledgement => RetryClass::Reconnect,
        ChannelAdapterErrorKindV2::Authentication
        | ChannelAdapterErrorKindV2::Permission
        | ChannelAdapterErrorKindV2::MalformedEvent
        | ChannelAdapterErrorKindV2::PermanentDestination
        | ChannelAdapterErrorKindV2::UnsupportedFeature
        | ChannelAdapterErrorKindV2::StaleCursor
        | ChannelAdapterErrorKindV2::Cancelled => RetryClass::Permanent,
    };
    AdapterFailure {
        class,
        safe_message: error.safe_message,
        retry_after_ms: error.retry_after_ms,
    }
}

pub(crate) fn event_v2_to_v1(channel: &str, event: ChannelEventV2) -> Option<AdapterEvent> {
    let event_id = event.event_id;
    match event.event {
        ChannelEventKindV2::MessageCreated(message) => {
            Some(AdapterEvent::Inbound(Box::new(InboundMessage {
                channel: channel.to_owned(),
                external_account: message.account_id,
                conversation: message.conversation.platform_id,
                thread: message.conversation.thread_id,
                sender: message.sender.platform_id,
                message_id: message.message_id,
                reply_target: message.conversation.reply_to_message_id,
                text: message.text,
                attachments: message
                    .attachments
                    .into_iter()
                    .map(|attachment| attachment.attachment)
                    .collect(),
                occurred_at: message.occurred_at,
                intent: InboundIntent::Prompt,
            })))
        }
        ChannelEventKindV2::MessageEdited(edit) => {
            Some(AdapterEvent::Inbound(Box::new(InboundMessage {
                channel: channel.to_owned(),
                external_account: edit.account_id,
                conversation: edit.conversation.platform_id,
                thread: edit.conversation.thread_id,
                sender: edit.editor.platform_id,
                message_id: event_id,
                reply_target: Some(edit.message_id),
                text: format!("[{channel} message edited]\n{}", edit.text),
                attachments: Vec::new(),
                occurred_at: edit.occurred_at,
                intent: InboundIntent::Prompt,
            })))
        }
        ChannelEventKindV2::MessageDeleted(deletion) => {
            Some(AdapterEvent::Inbound(Box::new(InboundMessage {
                channel: channel.to_owned(),
                external_account: deletion.account_id,
                conversation: deletion.conversation.platform_id,
                thread: deletion.conversation.thread_id,
                sender: deletion
                    .actor
                    .map_or_else(|| "unknown".to_owned(), |actor| actor.platform_id),
                message_id: event_id,
                reply_target: Some(deletion.message_id),
                text: format!("[{channel} message deleted]"),
                attachments: Vec::new(),
                occurred_at: deletion.occurred_at,
                intent: InboundIntent::Prompt,
            })))
        }
        ChannelEventKindV2::Reaction(reaction) => {
            Some(AdapterEvent::Inbound(Box::new(InboundMessage {
                channel: channel.to_owned(),
                external_account: reaction.account_id,
                conversation: reaction.conversation.platform_id,
                thread: reaction.conversation.thread_id,
                sender: reaction.actor.platform_id,
                message_id: event_id,
                reply_target: Some(reaction.message_id),
                text: format!(
                    "[{channel} reaction {:?}: {}]",
                    reaction.action, reaction.reaction
                ),
                attachments: Vec::new(),
                occurred_at: reaction.occurred_at,
                intent: InboundIntent::Prompt,
            })))
        }
        ChannelEventKindV2::Command(command) => {
            Some(AdapterEvent::Inbound(Box::new(InboundMessage {
                channel: channel.to_owned(),
                external_account: command.account_id,
                conversation: command.conversation.platform_id,
                thread: command.conversation.thread_id,
                sender: command.sender.platform_id,
                message_id: event_id,
                reply_target: command.conversation.reply_to_message_id,
                text: format!("{} {}", command.name, command.arguments)
                    .trim_end()
                    .to_owned(),
                attachments: Vec::new(),
                occurred_at: command.occurred_at,
                intent: InboundIntent::Prompt,
            })))
        }
        ChannelEventKindV2::RateLimited { retry_after_ms } => {
            Some(AdapterEvent::RateLimited { retry_after_ms })
        }
        ChannelEventKindV2::ReconnectRequired { safe_reason } => {
            Some(AdapterEvent::Disconnected { safe_reason })
        }
        ChannelEventKindV2::Typing { .. }
        | ChannelEventKindV2::Receipt { .. }
        | ChannelEventKindV2::CancellationRequested { .. } => None,
    }
}

fn required(value: Option<String>, message: &str) -> Result<String, AdapterFailure> {
    value
        .filter(|value| !value.trim().is_empty())
        .ok_or_else(|| permanent(message))
}

pub(crate) fn now() -> UtcTimestamp {
    UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH)
}

pub(crate) fn parse_timestamp(value: &str) -> Option<UtcTimestamp> {
    DateTime::parse_from_rfc3339(value)
        .ok()
        .map(|timestamp| UtcTimestamp::from_unix_millis(timestamp.timestamp_millis()))
}

pub(crate) fn credential_belongs_to(reference: &CredentialRef, account: &str) -> bool {
    matches!(&reference.owner, CredentialOwner::Channel(owner) if owner == account)
}

pub(crate) fn allowed_endpoint(url: &str) -> bool {
    url.starts_with("https://")
        || url.starts_with("http://127.0.0.1:")
        || url.starts_with("http://localhost:")
}

fn allowed_teams_service_url(url: &str) -> bool {
    let Some(remainder) = url.strip_prefix("https://") else {
        return false;
    };
    let authority = remainder.split('/').next().unwrap_or_default();
    if authority.is_empty() || authority.contains('@') || authority.contains(':') {
        return false;
    }
    matches!(
        authority.to_ascii_lowercase().as_str(),
        "smba.trafficmanager.net"
            | "smba.infra.gcc.teams.microsoft.com"
            | "smba.infra.gov.teams.microsoft.us"
            | "smba.infra.dod.teams.microsoft.us"
    )
}

pub(crate) fn bounded_recent(values: &[String], capacity: usize) -> VecDeque<String> {
    values
        .iter()
        .rev()
        .take(capacity)
        .cloned()
        .collect::<Vec<_>>()
        .into_iter()
        .rev()
        .collect()
}

pub(crate) fn percent_encode(value: &str) -> String {
    let mut encoded = String::new();
    for byte in value.bytes() {
        if byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.' | b'~') {
            encoded.push(char::from(byte));
        } else {
            use std::fmt::Write as _;
            let _ = write!(encoded, "%{byte:02X}");
        }
    }
    encoded
}

pub(crate) fn permanent(message: &str) -> AdapterFailure {
    AdapterFailure {
        class: RetryClass::Permanent,
        safe_message: message.to_owned(),
        retry_after_ms: None,
    }
}

pub(crate) fn retryable(message: &str) -> AdapterFailure {
    AdapterFailure {
        class: RetryClass::Retryable,
        safe_message: message.to_owned(),
        retry_after_ms: None,
    }
}

pub(crate) fn reconnect_failure(message: &str) -> AdapterFailure {
    AdapterFailure {
        class: RetryClass::Reconnect,
        safe_message: message.to_owned(),
        retry_after_ms: None,
    }
}

pub(crate) fn rate_limited(message: &str, retry_after_ms: Option<u64>) -> AdapterFailure {
    AdapterFailure {
        class: RetryClass::RateLimited,
        safe_message: message.to_owned(),
        retry_after_ms,
    }
}

pub(crate) fn transport_error(platform: &str, error: &ureq::Error) -> AdapterFailure {
    match error {
        ureq::Error::Timeout(_) => retryable(&format!("{platform} request timed out")),
        _ => reconnect_failure(&format!("{platform} transport failed")),
    }
}
