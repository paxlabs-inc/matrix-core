#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet, VecDeque};
use std::fmt::Display;
use std::time::Duration;

use keith_agent_types::{ProfileId, UtcTimestamp};
use keith_channel_core::{
    AdapterCapability, AdapterEvent, AdapterFailure, AdapterFeatures, Attachment,
    ChannelAccountSetupV2, ChannelAdapter, ChannelAdapterErrorKindV2, ChannelAdapterErrorV2,
    ChannelAdapterV2, ChannelAttachmentKindV2, ChannelAttachmentV2, ChannelCapabilitiesV2,
    ChannelCapabilitySupportV2, ChannelCapabilityV2, ChannelConnectionHealthV2,
    ChannelContractVersion, ChannelConversationKindV2, ChannelConversationV2, ChannelEventKindV2,
    ChannelEventV2, ChannelIdentityV2, ChannelMessageV2, ChannelOperationReceiptV2,
    ChannelOperationV2, ChannelOutboundMessageV2, ChannelReceiptStateV2, ChannelRichContentV2,
    OutboundMessage, ReconnectCursorV2, SendReceipt,
};
use keith_credentials::{CredentialRef, SecretValue};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use crate::teams::{
    PreparedHttpRequest, adapter_failure_from_v2, allowed_endpoint, bounded_recent,
    classify_test_status, credential_belongs_to, event_v2_to_v1, now, operation_receipt,
    parse_timestamp, permanent, rate_limited, retryable, transport_error, v2_error,
    valid_channel_identity,
};

const GOOGLE_ACCOUNTS_ISSUER: &str = "https://accounts.google.com";
const GOOGLE_ACCOUNTS_ISSUER_ALIAS: &str = "accounts.google.com";
const GOOGLE_CHAT_SERVICE_ACCOUNT: &str = "chat@system.gserviceaccount.com";

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct GoogleChatConfig {
    pub api_base: String,
    pub authentication_audience: String,
    pub external_account: String,
    pub profile_id: ProfileId,
    pub credential_ref: CredentialRef,
    pub max_event_bytes: usize,
    pub max_attachment_bytes: u64,
    pub timeout_ms: u64,
    pub deduplication_capacity: usize,
}

impl GoogleChatConfig {
    pub fn production(
        authentication_audience: impl Into<String>,
        external_account: impl Into<String>,
        profile_id: ProfileId,
        credential_ref: CredentialRef,
    ) -> Self {
        Self {
            api_base: "https://chat.googleapis.com".to_owned(),
            authentication_audience: authentication_audience.into(),
            external_account: external_account.into(),
            profile_id,
            credential_ref,
            max_event_bytes: 1024 * 1024,
            max_attachment_bytes: 25 * 1024 * 1024,
            timeout_ms: 30_000,
            deduplication_capacity: 4096,
        }
    }
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct GoogleChatCursor {
    pub recent_event_ids: Vec<String>,
    #[serde(default)]
    pub last_event_at: Option<UtcTimestamp>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct GoogleChatVerifiedClaims {
    pub issuer: String,
    pub audience: String,
    pub subject: String,
    pub email: Option<String>,
    pub expires_at: UtcTimestamp,
}

/// Verifies Google-issued ID tokens or project-number JWTs before JSON decoding.
pub trait GoogleChatRequestVerifier {
    type Error: Display;

    /// # Errors
    ///
    /// Returns an error when the authorization token is invalid, expired, or for another
    /// audience.
    fn verify(
        &self,
        authorization: &str,
        expected_audience: &str,
        now: UtcTimestamp,
    ) -> Result<GoogleChatVerifiedClaims, Self::Error>;
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum GoogleChatIngestOutcome {
    Queued,
    Duplicate,
    Ignored,
}

#[derive(Clone, Debug, Eq, PartialEq)]
#[allow(clippy::struct_excessive_bools)]
pub struct GoogleChatCapabilities {
    pub messages: bool,
    pub replies: bool,
    pub spaces: bool,
    pub threads: bool,
    pub inbound_cards: bool,
    pub outbound_cards: bool,
    pub inbound_attachments: bool,
    pub outbound_attachments: bool,
    pub reconnect: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct GoogleChatSetupDiagnostics {
    pub profile_id: ProfileId,
    pub external_account: String,
    pub credential_ref: CredentialRef,
    pub callback_verified: bool,
    pub revoked: bool,
    pub cursor_event_count: usize,
    pub capabilities: GoogleChatCapabilities,
    pub safe_error: Option<String>,
}

pub struct GoogleChatAdapter {
    config: GoogleChatConfig,
    access_token: SecretValue,
    http: ureq::Agent,
    cursor: GoogleChatCursor,
    seen: BTreeSet<String>,
    seen_order: VecDeque<String>,
    inbound: VecDeque<ChannelEventV2>,
    cancelled: BTreeSet<String>,
    callback_verified: bool,
    revoked: bool,
    safe_error: Option<String>,
}

impl GoogleChatAdapter {
    /// # Errors
    ///
    /// Returns a permanent error for invalid endpoints, bounds, audience, or credential scope.
    pub fn new(
        config: GoogleChatConfig,
        access_token: SecretValue,
        cursor: GoogleChatCursor,
    ) -> Result<Self, AdapterFailure> {
        if !valid_channel_identity(&config.authentication_audience)
            || !valid_channel_identity(&config.external_account)
            || !allowed_endpoint(&config.api_base)
            || config.max_event_bytes == 0
            || config.max_attachment_bytes == 0
            || config.timeout_ms == 0
            || config.deduplication_capacity == 0
            || !credential_belongs_to(&config.credential_ref, &config.external_account)
            || cursor
                .recent_event_ids
                .iter()
                .any(|event_id| space_from_message_name(event_id).is_none())
        {
            return Err(permanent("invalid Google Chat adapter configuration"));
        }
        let http: ureq::Agent = ureq::Agent::config_builder()
            .timeout_global(Some(Duration::from_millis(config.timeout_ms)))
            .http_status_as_error(false)
            .build()
            .into();
        let seen_order = bounded_recent(&cursor.recent_event_ids, config.deduplication_capacity);
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

    pub const fn cursor(&self) -> &GoogleChatCursor {
        &self.cursor
    }

    #[allow(clippy::unused_self)]
    pub fn capabilities(&self) -> GoogleChatCapabilities {
        GoogleChatCapabilities {
            messages: true,
            replies: true,
            spaces: true,
            threads: true,
            inbound_cards: true,
            outbound_cards: false,
            inbound_attachments: true,
            outbound_attachments: false,
            reconnect: true,
        }
    }

    pub fn setup_diagnostics(&self) -> GoogleChatSetupDiagnostics {
        GoogleChatSetupDiagnostics {
            profile_id: self.config.profile_id.clone(),
            external_account: self.config.external_account.clone(),
            credential_ref: self.config.credential_ref.clone(),
            callback_verified: self.callback_verified,
            revoked: self.revoked,
            cursor_event_count: self.cursor.recent_event_ids.len(),
            capabilities: self.capabilities(),
            safe_error: self.safe_error.clone(),
        }
    }

    pub fn account_setup_v2(&self) -> ChannelAccountSetupV2 {
        ChannelAccountSetupV2 {
            account_id: self.config.external_account.clone(),
            required_credential_names: BTreeSet::from([
                "chat_api_access_token".to_owned(),
                "google_chat_request_verifier".to_owned(),
            ]),
            required_scopes: BTreeSet::from(
                ["https://www.googleapis.com/auth/chat.bot".to_owned()],
            ),
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
            reconnect_cursor_present: !self.cursor.recent_event_ids.is_empty(),
            safe_test_supported: self
                .cursor
                .recent_event_ids
                .iter()
                .any(|id| id.starts_with("spaces/") && id.contains("/messages/")),
            metadata: BTreeMap::from([
                (
                    "authentication_audience".to_owned(),
                    self.config.authentication_audience.clone(),
                ),
                (
                    "callback_verified".to_owned(),
                    self.callback_verified.to_string(),
                ),
            ]),
        }
    }

    /// Prepares the read-only Google Chat space lookup used to test this account.
    ///
    /// # Errors
    ///
    /// Returns a permanent failure until a verified message identifies a space.
    pub fn prepare_test_connection(&self) -> Result<PreparedHttpRequest, AdapterFailure> {
        self.ensure_active()?;
        let space = self
            .cursor
            .recent_event_ids
            .iter()
            .rev()
            .find_map(|event_id| space_from_message_name(event_id).map(str::to_owned))
            .ok_or_else(|| permanent("Google Chat safe test needs a verified space"))?;
        Ok(PreparedHttpRequest {
            method: "GET".to_owned(),
            url: format!("{}/v1/{space}", self.config.api_base.trim_end_matches('/')),
            content_type: "application/json".to_owned(),
            body: Vec::new(),
            idempotency_key: None,
        })
    }

    /// Runs a read-only Google Chat space lookup using the scoped API credential.
    ///
    /// # Errors
    ///
    /// Returns a classified authentication, permission, rate-limit, transport, or destination
    /// failure.
    pub fn test_connection(&self) -> Result<(), ChannelAdapterErrorV2> {
        let request = self
            .prepare_test_connection()
            .map_err(ChannelAdapterErrorV2::from)?;
        let response = self.access_token.with_bytes(|token| {
            let token = std::str::from_utf8(token).map_err(|_| {
                v2_error(
                    ChannelAdapterErrorKindV2::Authentication,
                    "Google Chat access token is not UTF-8",
                )
            })?;
            self.http
                .get(&request.url)
                .header("Authorization", format!("Bearer {token}"))
                .call()
                .map_err(|error| {
                    ChannelAdapterErrorV2::from(transport_error("Google Chat", &error))
                })
        })?;
        classify_test_status("Google Chat", response.status().as_u16())
    }

    /// Authenticates a Google Chat interaction before parsing and normalizing it.
    ///
    /// # Errors
    ///
    /// Returns a classified failure when authentication, claims, bounds, or payload validation
    /// fails.
    #[allow(clippy::too_many_lines)]
    pub fn ingest_verified_event<V: GoogleChatRequestVerifier>(
        &mut self,
        authorization: &str,
        body: &[u8],
        verifier: &V,
    ) -> Result<GoogleChatIngestOutcome, AdapterFailure> {
        self.ensure_active()?;
        let current_time = now();
        let claims = verifier
            .verify(
                authorization,
                &self.config.authentication_audience,
                current_time,
            )
            .map_err(|error| {
                self.safe_error = Some("Google Chat request authentication failed".to_owned());
                AdapterFailure {
                    class: keith_channel_core::RetryClass::Permanent,
                    safe_message: format!("Google Chat request authentication failed: {error}"),
                    retry_after_ms: None,
                }
            })?;
        let recognized_issuer = matches!(
            claims.issuer.as_str(),
            GOOGLE_ACCOUNTS_ISSUER | GOOGLE_ACCOUNTS_ISSUER_ALIAS | GOOGLE_CHAT_SERVICE_ACCOUNT
        );
        let recognized_identity = claims.email.as_deref() == Some(GOOGLE_CHAT_SERVICE_ACCOUNT)
            || claims.issuer == GOOGLE_CHAT_SERVICE_ACCOUNT;
        if !recognized_issuer
            || !recognized_identity
            || claims.audience != self.config.authentication_audience
            || claims.subject.trim().is_empty()
            || claims.expires_at <= current_time
        {
            self.safe_error = Some("Google Chat request claims were rejected".to_owned());
            return Err(permanent("Google Chat request claims were rejected"));
        }
        self.callback_verified = true;
        if body.len() > self.config.max_event_bytes {
            return Err(permanent("Google Chat event exceeds the configured limit"));
        }
        let event: GoogleChatEvent = serde_json::from_slice(body)
            .map_err(|_| permanent("Google Chat event is malformed"))?;
        if event.event_type != "MESSAGE" && event.event_type != "ADDED_TO_SPACE" {
            return Ok(GoogleChatIngestOutcome::Ignored);
        }
        let message = event
            .message
            .ok_or_else(|| permanent("Google Chat event has no message"))?;
        let message_id = required(message.name, "Google Chat message has no name")?;
        if self.seen.contains(&message_id) {
            return Ok(GoogleChatIngestOutcome::Duplicate);
        }
        let conversation = required(
            event.space.and_then(|space| space.name),
            "Google Chat event has no space",
        )?;
        if !valid_space_name(&conversation)
            || space_from_message_name(&message_id) != Some(conversation.as_str())
        {
            return Err(permanent("Google Chat space name is malformed"));
        }
        let sender = message
            .sender
            .and_then(|sender| sender.name)
            .or_else(|| event.user.and_then(|user| user.name))
            .filter(|value| !value.trim().is_empty())
            .ok_or_else(|| permanent("Google Chat event has no sender"))?;
        let attachments =
            normalize_attachments(message.attachments, self.config.max_attachment_bytes)?;
        let text = message.text.unwrap_or_default();
        if text.trim().is_empty() && attachments.is_empty() && message.cards_v2.is_empty() {
            return Err(permanent("Google Chat event has no message content"));
        }
        let thread = message.thread.and_then(|thread| thread.name);
        if thread
            .as_deref()
            .is_some_and(|thread| !valid_thread_name(thread, &conversation))
        {
            return Err(permanent("Google Chat thread name is malformed"));
        }
        let occurred_at = event
            .event_time
            .as_deref()
            .and_then(parse_timestamp)
            .or_else(|| message.create_time.as_deref().and_then(parse_timestamp))
            .unwrap_or(current_time);
        self.remember(message_id.clone(), occurred_at);
        let event = ChannelEventV2 {
            contract: ChannelContractVersion::new(2, 0),
            event_id: message_id.clone(),
            delivery_attempt: 1,
            event: ChannelEventKindV2::MessageCreated(ChannelMessageV2 {
                message_id: message_id.clone(),
                account_id: self.config.external_account.clone(),
                conversation: ChannelConversationV2 {
                    platform_id: conversation,
                    kind: ChannelConversationKindV2::Channel,
                    thread_id: thread,
                    reply_to_message_id: None,
                },
                sender: ChannelIdentityV2 {
                    platform_id: sender,
                    display_name: None,
                    is_bot: false,
                },
                text,
                attachments,
                rich_content: message
                    .cards_v2
                    .into_iter()
                    .map(|card| ChannelRichContentV2 {
                        kind: "google_chat_card_v2".to_owned(),
                        text: card.to_string(),
                        metadata: std::collections::BTreeMap::new(),
                    })
                    .collect(),
                mentions: Vec::new(),
                occurred_at,
                metadata: std::collections::BTreeMap::from([(
                    "platform".to_owned(),
                    "google_chat".to_owned(),
                )]),
            }),
            metadata: std::collections::BTreeMap::new(),
        };
        event
            .validate(&self.capabilities_v2())
            .map_err(adapter_failure_from_v2)?;
        self.inbound.push_back(event);
        self.safe_error = None;
        Ok(GoogleChatIngestOutcome::Queued)
    }

    pub fn revoke(&mut self) {
        self.revoked = true;
        self.inbound.clear();
        self.safe_error = Some("Google Chat account was revoked".to_owned());
    }

    /// Prepares the exact non-secret Google Chat API request used by [`Self::send`].
    ///
    /// # Errors
    ///
    /// Returns a permanent failure for cross-account routes, unsupported media, or encoding.
    pub fn prepare_send(
        &self,
        message: &OutboundMessage,
    ) -> Result<PreparedHttpRequest, AdapterFailure> {
        self.ensure_active()?;
        if message.route.channel != "google_chat"
            || message.route.external_account != self.config.external_account
            || !valid_space_name(&message.route.conversation)
            || message
                .route
                .thread
                .as_deref()
                .is_some_and(|thread| !valid_thread_name(thread, &message.route.conversation))
            || message.text.trim().is_empty()
        {
            return Err(permanent(
                "Google Chat delivery route belongs to another adapter account",
            ));
        }
        if !message.artifacts.is_empty() {
            return Err(permanent(
                "Google Chat outbound attachments require user-authenticated media upload",
            ));
        }
        let url = format!(
            "{}/v1/{}/messages?messageReplyOption=REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD",
            self.config.api_base.trim_end_matches('/'),
            message.route.conversation
        );
        let mut payload = json!({"text": message.text});
        if let Some(thread) = &message.route.thread {
            payload["thread"] = json!({"name": thread});
        }
        Ok(PreparedHttpRequest {
            method: "POST".to_owned(),
            url,
            content_type: "application/json".to_owned(),
            body: serde_json::to_vec(&payload)
                .map_err(|_| permanent("Google Chat outbound message could not be encoded"))?,
            idempotency_key: Some(message.idempotency_key.clone()),
        })
    }

    fn ensure_active(&self) -> Result<(), AdapterFailure> {
        if self.revoked {
            Err(permanent("Google Chat account was revoked"))
        } else {
            Ok(())
        }
    }

    fn remember(&mut self, event_id: String, occurred_at: UtcTimestamp) {
        self.seen.insert(event_id.clone());
        self.seen_order.push_back(event_id);
        while self.seen_order.len() > self.config.deduplication_capacity {
            if let Some(expired) = self.seen_order.pop_front() {
                self.seen.remove(&expired);
            }
        }
        self.cursor.recent_event_ids = self.seen_order.iter().cloned().collect();
        self.cursor.last_event_at = Some(occurred_at);
    }

    fn send_message(&mut self, message: &OutboundMessage) -> Result<SendReceipt, AdapterFailure> {
        let request = self.prepare_send(message)?;
        let mut response = self.access_token.with_bytes(|token| {
            let token = std::str::from_utf8(token)
                .map_err(|_| permanent("Google Chat access token is not UTF-8"))?;
            self.http
                .post(&request.url)
                .header("Authorization", format!("Bearer {token}"))
                .header("Content-Type", &request.content_type)
                .send(&request.body)
                .map_err(|error| transport_error("Google Chat", &error))
        })?;
        let status = response.status().as_u16();
        let response_body = response
            .body_mut()
            .with_config()
            .limit(u64::try_from(self.config.max_event_bytes).unwrap_or(u64::MAX))
            .read_to_vec()
            .map_err(|error| transport_error("Google Chat", &error))?;
        match status {
            200..=299 => {
                let receipt: GoogleChatSendReceipt = serde_json::from_slice(&response_body)
                    .map_err(|_| permanent("Google Chat returned a malformed send receipt"))?;
                Ok(SendReceipt {
                    platform_message_id: receipt.name,
                    accepted_at: now(),
                    duplicate_possible: true,
                })
            }
            401 | 403 => Err(permanent("Google Chat authentication or permission denied")),
            429 => Err(rate_limited("Google Chat rate limit reached", None)),
            500..=599 => Err(retryable("Google Chat service is temporarily unavailable")),
            _ => Err(permanent("Google Chat rejected the outbound message")),
        }
    }
}

impl ChannelAdapterV2 for GoogleChatAdapter {
    fn capabilities_v2(&self) -> ChannelCapabilitiesV2 {
        google_chat_capabilities(&self.config)
    }

    fn receive_v2(&mut self) -> Result<ChannelEventV2, ChannelAdapterErrorV2> {
        self.ensure_active().map_err(ChannelAdapterErrorV2::from)?;
        self.inbound.pop_front().ok_or_else(|| {
            v2_error(
                ChannelAdapterErrorKindV2::TransientNetwork,
                "Google Chat has no pending verified event",
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
                        "Google Chat outbound cards are not enabled by this adapter",
                    ));
                }
                if self.cancelled.contains(&message.idempotency_key) {
                    return Err(v2_error(
                        ChannelAdapterErrorKindV2::Cancelled,
                        "Google Chat delivery was cancelled before dispatch",
                    ));
                }
                let receipt = self
                    .send_message(&OutboundMessage {
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
                        "Google Chat cancellation identity is empty",
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
                "Google Chat operation is not enabled by this adapter",
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
            .recent_event_ids
            .last()
            .zip(self.cursor.last_event_at)
            .map(|(value, observed_at)| ReconnectCursorV2 {
                value: value.clone(),
                observed_at,
            })
    }
}

impl ChannelAdapter for GoogleChatAdapter {
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
            if let Some(event) = event_v2_to_v1("google_chat", event) {
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
struct GoogleChatEvent {
    #[serde(rename = "type")]
    event_type: String,
    #[serde(rename = "eventTime")]
    event_time: Option<String>,
    space: Option<GoogleChatNamedResource>,
    user: Option<GoogleChatNamedResource>,
    message: Option<GoogleChatMessage>,
}

#[derive(Debug, Deserialize)]
struct GoogleChatMessage {
    name: Option<String>,
    sender: Option<GoogleChatNamedResource>,
    text: Option<String>,
    thread: Option<GoogleChatNamedResource>,
    #[serde(rename = "createTime")]
    create_time: Option<String>,
    #[serde(default)]
    #[serde(rename = "attachment")]
    attachments: Vec<GoogleChatAttachment>,
    #[serde(default)]
    #[serde(rename = "cardsV2")]
    cards_v2: Vec<Value>,
}

#[derive(Debug, Deserialize)]
struct GoogleChatNamedResource {
    name: Option<String>,
}

#[derive(Debug, Deserialize)]
struct GoogleChatAttachment {
    #[serde(rename = "contentName")]
    content_name: Option<String>,
    #[serde(rename = "contentType")]
    content_type: Option<String>,
    #[serde(rename = "attachmentDataRef")]
    data_ref: Option<GoogleChatAttachmentRef>,
    #[serde(rename = "downloadUri")]
    download_uri: Option<String>,
    size: Option<u64>,
}

#[derive(Debug, Deserialize)]
struct GoogleChatAttachmentRef {
    #[serde(rename = "resourceName")]
    resource_name: Option<String>,
}

#[derive(Debug, Deserialize)]
struct GoogleChatSendReceipt {
    name: String,
}

fn normalize_attachments(
    values: Vec<GoogleChatAttachment>,
    max_attachment_bytes: u64,
) -> Result<Vec<ChannelAttachmentV2>, AdapterFailure> {
    values
        .into_iter()
        .enumerate()
        .map(|(index, value)| {
            let byte_length = value.size.unwrap_or(0);
            if byte_length > max_attachment_bytes {
                return Err(permanent(
                    "Google Chat attachment exceeds the configured limit",
                ));
            }
            let resource_name = value
                .data_ref
                .and_then(|reference| reference.resource_name)
                .unwrap_or_else(|| format!("attachments/{index}"));
            let media_type = value
                .content_type
                .unwrap_or_else(|| "application/octet-stream".to_owned());
            let kind = if media_type.starts_with("image/") {
                ChannelAttachmentKindV2::Image
            } else if media_type.starts_with("audio/") {
                ChannelAttachmentKindV2::Audio
            } else if media_type.starts_with("video/") {
                ChannelAttachmentKindV2::Video
            } else {
                ChannelAttachmentKindV2::File
            };
            Ok(ChannelAttachmentV2 {
                attachment: Attachment {
                    id: resource_name,
                    file_name: value
                        .content_name
                        .unwrap_or_else(|| format!("attachment-{index}")),
                    media_type,
                    byte_length,
                    artifact_id: None,
                    download_url: value.download_uri,
                    staging_file: None,
                    sha256: None,
                },
                kind,
                duration_ms: None,
                metadata: std::collections::BTreeMap::new(),
            })
        })
        .collect()
}

fn required(value: Option<String>, message: &str) -> Result<String, AdapterFailure> {
    value
        .filter(|value| !value.trim().is_empty())
        .ok_or_else(|| permanent(message))
}

fn valid_resource_component(value: &str) -> bool {
    !value.is_empty()
        && !value.contains(['/', '?', '#', '\\'])
        && !value.chars().any(char::is_whitespace)
        && !value.chars().any(char::is_control)
}

fn valid_space_name(value: &str) -> bool {
    value
        .strip_prefix("spaces/")
        .is_some_and(valid_resource_component)
}

fn space_from_message_name(value: &str) -> Option<&str> {
    let (space, message) = value.rsplit_once("/messages/")?;
    (valid_space_name(space) && valid_resource_component(message)).then_some(space)
}

fn valid_thread_name(value: &str, space: &str) -> bool {
    value
        .strip_prefix(space)
        .and_then(|suffix| suffix.strip_prefix("/threads/"))
        .is_some_and(valid_resource_component)
}

fn google_chat_capabilities(config: &GoogleChatConfig) -> ChannelCapabilitiesV2 {
    let unsupported = |reason: &str| ChannelCapabilitySupportV2::Unsupported {
        safe_reason: reason.to_owned(),
    };
    let mut declarations = ChannelCapabilityV2::ALL
        .into_iter()
        .map(|capability| {
            (
                capability,
                unsupported("Google Chat capability is not enabled by this adapter"),
            )
        })
        .collect::<BTreeMap<_, _>>();
    for capability in [
        ChannelCapabilityV2::InboundMessages,
        ChannelCapabilityV2::OutboundMessages,
        ChannelCapabilityV2::Threads,
        ChannelCapabilityV2::Replies,
        ChannelCapabilityV2::Attachments,
        ChannelCapabilityV2::RateLimits,
        ChannelCapabilityV2::Reconnect,
        ChannelCapabilityV2::Cancellation,
    ] {
        declarations.insert(capability, ChannelCapabilitySupportV2::Supported);
    }
    declarations.insert(
        ChannelCapabilityV2::Mentions,
        unsupported("Google Chat mention annotations are not projected by this adapter"),
    );
    declarations.insert(
        ChannelCapabilityV2::Commands,
        unsupported("Google Chat app commands are not projected by this adapter"),
    );
    declarations.insert(
        ChannelCapabilityV2::MessageEdits,
        unsupported("Google Chat message updates require separately granted write scopes"),
    );
    declarations.insert(
        ChannelCapabilityV2::MessageDeletion,
        unsupported("Google Chat message deletion requires separately granted write scopes"),
    );
    declarations.insert(
        ChannelCapabilityV2::Reactions,
        unsupported("Google Chat app reactions require user-authenticated scopes"),
    );
    declarations.insert(
        ChannelCapabilityV2::Voice,
        unsupported("Google Chat audio arrives as a file attachment, not native voice"),
    );
    declarations.insert(
        ChannelCapabilityV2::RichContent,
        unsupported("Google Chat cards are accepted inbound but outbound cards are not enabled"),
    );
    declarations.insert(
        ChannelCapabilityV2::Typing,
        unsupported("Google Chat API does not expose bot typing state"),
    );
    declarations.insert(
        ChannelCapabilityV2::DeliveryReceipts,
        unsupported("Google Chat does not expose bot delivery receipts here"),
    );
    declarations.insert(
        ChannelCapabilityV2::ReadReceipts,
        unsupported("Google Chat read-state scopes are not granted to this adapter"),
    );
    declarations.insert(
        ChannelCapabilityV2::IdempotentSend,
        unsupported("Google Chat send acknowledgement can be uncertain"),
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
