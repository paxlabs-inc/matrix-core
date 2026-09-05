#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet, VecDeque};
use std::fmt::Display;
use std::time::Duration;

use keith_agent_types::{ArtifactId, ProfileId, UtcTimestamp};
use keith_channel_core::{
    AdapterCapability, AdapterEvent, AdapterFailure, AdapterFeatures, Attachment,
    ChannelAccountSetupV2, ChannelAdapter, ChannelAdapterErrorKindV2, ChannelAdapterErrorV2,
    ChannelAdapterV2, ChannelAttachmentKindV2, ChannelAttachmentV2, ChannelCapabilitiesV2,
    ChannelCapabilitySupportV2, ChannelCapabilityV2, ChannelConnectionHealthV2,
    ChannelContractVersion, ChannelConversationKindV2, ChannelConversationV2, ChannelEventKindV2,
    ChannelEventV2, ChannelIdentityV2, ChannelMessageV2, ChannelOperationReceiptV2,
    ChannelOperationV2, ChannelOutboundMessageV2, ChannelReceiptStateV2, OutboundMessage,
    ReconnectCursorV2, RetryClass, SendReceipt,
};
use keith_credentials::{CredentialRef, SecretValue};
use serde::{Deserialize, Serialize};
use serde_json::json;

use crate::teams::{
    PreparedHttpRequest, adapter_failure_from_v2, allowed_endpoint, bounded_recent,
    classify_test_status, credential_belongs_to, event_v2_to_v1, now, operation_receipt,
    parse_timestamp, permanent, rate_limited, retryable, transport_error, v2_error,
    valid_channel_identity,
};

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct EmailConfig {
    pub provider_name: String,
    pub provider_api_base: String,
    pub webhook_audience: String,
    pub channel_account: String,
    pub sender_address: String,
    pub profile_id: ProfileId,
    pub credential_ref: CredentialRef,
    pub account_test_path: String,
    pub provider_guarantees_idempotency: bool,
    pub max_event_bytes: usize,
    pub max_attachment_bytes: u64,
    pub max_staged_bytes: usize,
    pub timeout_ms: u64,
    pub deduplication_capacity: usize,
}

impl EmailConfig {
    pub fn provider(
        provider_name: impl Into<String>,
        provider_api_base: impl Into<String>,
        webhook_audience: impl Into<String>,
        channel_account: impl Into<String>,
        sender_address: impl Into<String>,
        profile_id: ProfileId,
        credential_ref: CredentialRef,
    ) -> Self {
        Self {
            provider_name: provider_name.into(),
            provider_api_base: provider_api_base.into(),
            webhook_audience: webhook_audience.into(),
            channel_account: channel_account.into(),
            sender_address: sender_address.into(),
            profile_id,
            credential_ref,
            account_test_path: "/account".to_owned(),
            provider_guarantees_idempotency: false,
            max_event_bytes: 10 * 1024 * 1024,
            max_attachment_bytes: 25 * 1024 * 1024,
            max_staged_bytes: 50 * 1024 * 1024,
            timeout_ms: 30_000,
            deduplication_capacity: 4096,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "state", content = "detail")]
pub enum EmailDeliveryState {
    Pending,
    Accepted { provider_message_id: String },
    PossibleDuplicate,
    Rejected,
    Cancelled,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EmailThreadContext {
    pub peer_address: String,
    pub subject: String,
    pub last_message_id: String,
    pub references: Vec<String>,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EmailCursor {
    pub recent_message_ids: Vec<String>,
    pub threads: BTreeMap<String, EmailThreadContext>,
    pub deliveries: BTreeMap<String, EmailDeliveryState>,
    #[serde(default)]
    pub last_event_at: Option<UtcTimestamp>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct EmailVerifiedEvent {
    pub provider: String,
    pub audience: String,
    pub event_id: String,
    pub expires_at: UtcTimestamp,
}

/// Validates the email provider's webhook signature before its JSON event is parsed.
pub trait EmailEventVerifier {
    type Error: Display;

    /// # Errors
    ///
    /// Returns a provider-specific signature, audience, expiry, or request validation error.
    fn verify(
        &self,
        headers: &[(String, String)],
        body: &[u8],
        expected_audience: &str,
        now: UtcTimestamp,
    ) -> Result<EmailVerifiedEvent, Self::Error>;
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum EmailIngestOutcome {
    Queued,
    Duplicate,
}

#[derive(Clone, Debug, Eq, PartialEq)]
#[allow(clippy::struct_excessive_bools)]
pub struct EmailCapabilities {
    pub provider_events: bool,
    pub provider_send: bool,
    pub threading: bool,
    pub attachments: bool,
    pub reply_identity: bool,
    pub idempotent_send: bool,
    pub possible_duplicate_tracking: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct EmailSetupDiagnostics {
    pub profile_id: ProfileId,
    pub channel_account: String,
    pub sender_address: String,
    pub provider_name: String,
    pub credential_ref: CredentialRef,
    pub callback_verified: bool,
    pub revoked: bool,
    pub tracked_threads: usize,
    pub possible_duplicate_count: usize,
    pub capabilities: EmailCapabilities,
    pub safe_error: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct EmailUpload {
    pub file_name: String,
    pub media_type: String,
    pub bytes: Vec<u8>,
}

pub struct EmailAdapter {
    config: EmailConfig,
    access_token: SecretValue,
    http: ureq::Agent,
    cursor: EmailCursor,
    seen: BTreeSet<String>,
    seen_order: VecDeque<String>,
    inbound: VecDeque<ChannelEventV2>,
    staged: BTreeMap<ArtifactId, EmailUpload>,
    staged_bytes: usize,
    callback_verified: bool,
    cancelled: BTreeSet<String>,
    revoked: bool,
    safe_error: Option<String>,
}

impl EmailAdapter {
    /// # Errors
    ///
    /// Returns a permanent error for invalid endpoints, addresses, bounds, or credential scope.
    pub fn new(
        config: EmailConfig,
        access_token: SecretValue,
        cursor: EmailCursor,
    ) -> Result<Self, AdapterFailure> {
        if !valid_channel_identity(&config.provider_name)
            || !valid_channel_identity(&config.webhook_audience)
            || !valid_channel_identity(&config.channel_account)
            || !valid_email_address(&config.sender_address)
            || !allowed_endpoint(&config.provider_api_base)
            || !valid_relative_api_path(&config.account_test_path)
            || config.max_event_bytes == 0
            || config.max_attachment_bytes == 0
            || config.max_staged_bytes == 0
            || config.timeout_ms == 0
            || config.deduplication_capacity == 0
            || !credential_belongs_to(&config.credential_ref, &config.channel_account)
        {
            return Err(permanent("invalid email adapter configuration"));
        }
        let http: ureq::Agent = ureq::Agent::config_builder()
            .timeout_global(Some(Duration::from_millis(config.timeout_ms)))
            .http_status_as_error(false)
            .build()
            .into();
        let seen_order = bounded_recent(&cursor.recent_message_ids, config.deduplication_capacity);
        let seen = seen_order.iter().cloned().collect();
        Ok(Self {
            config,
            access_token,
            http,
            cursor,
            seen,
            seen_order,
            inbound: VecDeque::new(),
            staged: BTreeMap::new(),
            staged_bytes: 0,
            callback_verified: false,
            cancelled: BTreeSet::new(),
            revoked: false,
            safe_error: None,
        })
    }

    pub const fn cursor(&self) -> &EmailCursor {
        &self.cursor
    }

    pub fn capabilities(&self) -> EmailCapabilities {
        EmailCapabilities {
            provider_events: true,
            provider_send: true,
            threading: true,
            attachments: true,
            reply_identity: true,
            idempotent_send: self.config.provider_guarantees_idempotency,
            possible_duplicate_tracking: true,
        }
    }

    pub fn setup_diagnostics(&self) -> EmailSetupDiagnostics {
        EmailSetupDiagnostics {
            profile_id: self.config.profile_id.clone(),
            channel_account: self.config.channel_account.clone(),
            sender_address: self.config.sender_address.clone(),
            provider_name: self.config.provider_name.clone(),
            credential_ref: self.config.credential_ref.clone(),
            callback_verified: self.callback_verified,
            revoked: self.revoked,
            tracked_threads: self.cursor.threads.len(),
            possible_duplicate_count: self
                .cursor
                .deliveries
                .values()
                .filter(|state| matches!(state, EmailDeliveryState::PossibleDuplicate))
                .count(),
            capabilities: self.capabilities(),
            safe_error: self.safe_error.clone(),
        }
    }

    pub fn account_setup_v2(&self) -> ChannelAccountSetupV2 {
        ChannelAccountSetupV2 {
            account_id: self.config.channel_account.clone(),
            required_credential_names: BTreeSet::from([
                "provider_access_token".to_owned(),
                "webhook_signature_verifier".to_owned(),
            ]),
            required_scopes: BTreeSet::new(),
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
            reconnect_cursor_present: !self.cursor.recent_message_ids.is_empty(),
            safe_test_supported: true,
            metadata: BTreeMap::from([
                ("provider".to_owned(), self.config.provider_name.clone()),
                (
                    "sender_address".to_owned(),
                    self.config.sender_address.clone(),
                ),
                (
                    "callback_verified".to_owned(),
                    self.callback_verified.to_string(),
                ),
                ("scope_model".to_owned(), "provider-defined".to_owned()),
            ]),
        }
    }

    /// Prepares the provider's read-only account-identity request.
    ///
    /// # Errors
    ///
    /// Returns a permanent failure for a revoked account or invalid configured path.
    pub fn prepare_test_connection(&self) -> Result<PreparedHttpRequest, AdapterFailure> {
        self.ensure_active()?;
        if !valid_relative_api_path(&self.config.account_test_path) {
            return Err(permanent("email provider account test path is invalid"));
        }
        Ok(PreparedHttpRequest {
            method: "GET".to_owned(),
            url: format!(
                "{}{}",
                self.config.provider_api_base.trim_end_matches('/'),
                self.config.account_test_path
            ),
            content_type: "application/json".to_owned(),
            body: Vec::new(),
            idempotency_key: None,
        })
    }

    /// Runs the configured provider's read-only account-identity request.
    ///
    /// # Errors
    ///
    /// Returns a classified failure when the credential cannot read the configured sender
    /// identity or resolves to a different account.
    pub fn test_connection(&self) -> Result<(), ChannelAdapterErrorV2> {
        let request = self
            .prepare_test_connection()
            .map_err(ChannelAdapterErrorV2::from)?;
        let mut response = self.access_token.with_bytes(|token| {
            let token = std::str::from_utf8(token).map_err(|_| {
                v2_error(
                    ChannelAdapterErrorKindV2::Authentication,
                    "email provider token is not UTF-8",
                )
            })?;
            self.http
                .get(&request.url)
                .header("Authorization", format!("Bearer {token}"))
                .call()
                .map_err(|error| {
                    ChannelAdapterErrorV2::from(transport_error("email provider", &error))
                })
        })?;
        classify_test_status("email provider", response.status().as_u16())?;
        let body = response
            .body_mut()
            .with_config()
            .limit(u64::try_from(self.config.max_event_bytes).unwrap_or(u64::MAX))
            .read_to_vec()
            .map_err(|error| {
                ChannelAdapterErrorV2::from(transport_error("email provider", &error))
            })?;
        let account: EmailProviderAccount = serde_json::from_slice(&body).map_err(|_| {
            ChannelAdapterErrorV2::malformed("email provider returned a malformed account identity")
        })?;
        if account.sender_address != self.config.sender_address {
            return Err(v2_error(
                ChannelAdapterErrorKindV2::Permission,
                "email provider resolved to a different sender identity",
            ));
        }
        Ok(())
    }

    /// Stages real attachment bytes for a bounded provider send.
    ///
    /// # Errors
    ///
    /// Returns a permanent failure when metadata or byte ceilings are invalid.
    pub fn stage_artifact(
        &mut self,
        id: ArtifactId,
        upload: EmailUpload,
    ) -> Result<(), AdapterFailure> {
        self.ensure_active()?;
        if upload.file_name.trim().is_empty()
            || upload.media_type.trim().is_empty()
            || upload.bytes.is_empty()
            || u64::try_from(upload.bytes.len()).unwrap_or(u64::MAX)
                > self.config.max_attachment_bytes
        {
            return Err(permanent("email attachment is invalid or oversized"));
        }
        let previous = self.staged.get(&id).map_or(0, |item| item.bytes.len());
        let next = self
            .staged_bytes
            .saturating_sub(previous)
            .saturating_add(upload.bytes.len());
        if next > self.config.max_staged_bytes {
            return Err(permanent("email staged attachment budget exceeded"));
        }
        self.staged.insert(id, upload);
        self.staged_bytes = next;
        Ok(())
    }

    /// Verifies a provider webhook before parsing its normalized RFC message fields.
    ///
    /// # Errors
    ///
    /// Returns a classified failure for signature, claims, bounds, address, threading, or media
    /// validation errors.
    #[allow(clippy::too_many_lines)]
    pub fn ingest_provider_event<V: EmailEventVerifier>(
        &mut self,
        headers: &[(String, String)],
        body: &[u8],
        verifier: &V,
    ) -> Result<EmailIngestOutcome, AdapterFailure> {
        self.ensure_active()?;
        let current_time = now();
        let verified_event = verifier
            .verify(headers, body, &self.config.webhook_audience, current_time)
            .map_err(|error| {
                self.safe_error = Some("email provider event authentication failed".to_owned());
                AdapterFailure {
                    class: RetryClass::Permanent,
                    safe_message: format!("email provider event authentication failed: {error}"),
                    retry_after_ms: None,
                }
            })?;
        if verified_event.provider != self.config.provider_name
            || verified_event.audience != self.config.webhook_audience
            || verified_event.event_id.trim().is_empty()
            || verified_event.expires_at <= current_time
        {
            self.safe_error = Some("email provider event claims were rejected".to_owned());
            return Err(permanent("email provider event claims were rejected"));
        }
        self.callback_verified = true;
        if body.len() > self.config.max_event_bytes {
            return Err(permanent(
                "email provider event exceeds the configured limit",
            ));
        }
        let event: EmailProviderMessage = serde_json::from_slice(body)
            .map_err(|_| permanent("email provider event is malformed"))?;
        let message_id = normalize_message_id(&event.message_id)
            .ok_or_else(|| permanent("email provider event has no valid Message-ID"))?;
        if self.seen.contains(&message_id) {
            return Ok(EmailIngestOutcome::Duplicate);
        }
        if !valid_email_address(&event.from)
            || !event.to.iter().any(|to| {
                to.eq_ignore_ascii_case(&self.config.sender_address)
                    || to == &self.config.channel_account
            })
        {
            return Err(permanent(
                "email provider event is addressed outside this account",
            ));
        }
        if event.subject.trim().is_empty() {
            return Err(permanent("email provider event has no subject"));
        }
        let subject = event.subject.clone();
        let attachments =
            normalize_attachments(event.attachments, self.config.max_attachment_bytes)?;
        let text = event
            .text_body
            .filter(|body| !body.trim().is_empty())
            .or(event.html_body)
            .unwrap_or_default();
        if text.trim().is_empty() && attachments.is_empty() {
            return Err(permanent("email provider event has no message content"));
        }
        let mut references = event
            .references
            .into_iter()
            .filter_map(|value| normalize_message_id(&value))
            .collect::<Vec<_>>();
        let in_reply_to = event.in_reply_to.as_deref().and_then(normalize_message_id);
        let conversation = references
            .first()
            .cloned()
            .or_else(|| in_reply_to.clone())
            .unwrap_or_else(|| message_id.clone());
        if !references.iter().any(|value| value == &message_id) {
            references.push(message_id.clone());
        }
        self.cursor.threads.insert(
            conversation.clone(),
            EmailThreadContext {
                peer_address: event.from.clone(),
                subject: subject.clone(),
                last_message_id: message_id.clone(),
                references,
            },
        );
        let occurred_at = event
            .received_at
            .as_deref()
            .and_then(parse_timestamp)
            .unwrap_or(current_time);
        self.remember(message_id.clone(), occurred_at);
        let event = ChannelEventV2 {
            contract: ChannelContractVersion::new(2, 0),
            event_id: message_id.clone(),
            delivery_attempt: 1,
            event: ChannelEventKindV2::MessageCreated(ChannelMessageV2 {
                message_id,
                account_id: self.config.channel_account.clone(),
                conversation: ChannelConversationV2 {
                    platform_id: conversation.clone(),
                    kind: ChannelConversationKindV2::Direct,
                    thread_id: Some(conversation),
                    reply_to_message_id: in_reply_to,
                },
                sender: ChannelIdentityV2 {
                    platform_id: event.from,
                    display_name: None,
                    is_bot: false,
                },
                text,
                attachments,
                rich_content: Vec::new(),
                mentions: Vec::new(),
                occurred_at,
                metadata: BTreeMap::from([
                    ("platform".to_owned(), "email".to_owned()),
                    ("subject".to_owned(), subject),
                ]),
            }),
            metadata: BTreeMap::new(),
        };
        event
            .validate(&self.capabilities_v2())
            .map_err(adapter_failure_from_v2)?;
        self.inbound.push_back(event);
        self.safe_error = None;
        Ok(EmailIngestOutcome::Queued)
    }

    pub fn delivery_state(&self, idempotency_key: &str) -> Option<&EmailDeliveryState> {
        self.cursor.deliveries.get(idempotency_key)
    }

    /// Prepares the exact non-secret provider request used by [`Self::send`].
    ///
    /// # Errors
    ///
    /// Returns a permanent failure for cross-account routes, missing thread identity, unstaged
    /// attachments, or encoding failure.
    pub fn prepare_send(
        &self,
        message: &OutboundMessage,
    ) -> Result<PreparedHttpRequest, AdapterFailure> {
        self.ensure_active()?;
        if message.route.channel != "email"
            || message.route.external_account != self.config.channel_account
            || message.idempotency_key.trim().is_empty()
        {
            return Err(permanent(
                "email delivery route belongs to another adapter account",
            ));
        }
        let context = self.cursor.threads.get(&message.route.conversation);
        let recipient = context
            .map(|value| value.peer_address.clone())
            .or_else(|| {
                valid_email_address(&message.route.conversation)
                    .then(|| message.route.conversation.clone())
            })
            .ok_or_else(|| permanent("email route has no valid recipient"))?;
        let subject =
            context.map_or_else(|| "Keith".to_owned(), |value| reply_subject(&value.subject));
        let mut headers = BTreeMap::new();
        if let Some(context) = context {
            headers.insert("In-Reply-To".to_owned(), context.last_message_id.clone());
            headers.insert("References".to_owned(), context.references.join(" "));
        }
        let attachments = message
            .artifacts
            .iter()
            .map(|id| {
                let upload = self
                    .staged
                    .get(id)
                    .ok_or_else(|| permanent("email artifact bytes were not staged"))?;
                Ok(json!({
                    "name": upload.file_name,
                    "content_type": upload.media_type,
                    "content_base64": base64_encode(&upload.bytes),
                }))
            })
            .collect::<Result<Vec<_>, AdapterFailure>>()?;
        let payload = json!({
            "from": self.config.sender_address,
            "to": [recipient],
            "subject": subject,
            "text": message.text,
            "headers": headers,
            "attachments": attachments,
        });
        Ok(PreparedHttpRequest {
            method: "POST".to_owned(),
            url: format!(
                "{}/messages",
                self.config.provider_api_base.trim_end_matches('/')
            ),
            content_type: "application/json".to_owned(),
            body: serde_json::to_vec(&payload)
                .map_err(|_| permanent("email provider payload could not be encoded"))?,
            idempotency_key: Some(message.idempotency_key.clone()),
        })
    }

    pub fn revoke(&mut self) {
        self.revoked = true;
        self.inbound.clear();
        self.staged.clear();
        self.staged_bytes = 0;
        self.safe_error = Some("email account was revoked".to_owned());
    }

    fn ensure_active(&self) -> Result<(), AdapterFailure> {
        if self.revoked {
            Err(permanent("email account was revoked"))
        } else {
            Ok(())
        }
    }

    fn remember(&mut self, message_id: String, occurred_at: UtcTimestamp) {
        self.seen.insert(message_id.clone());
        self.seen_order.push_back(message_id);
        while self.seen_order.len() > self.config.deduplication_capacity {
            if let Some(expired) = self.seen_order.pop_front() {
                self.seen.remove(&expired);
            }
        }
        self.cursor.recent_message_ids = self.seen_order.iter().cloned().collect();
        self.cursor.last_event_at = Some(occurred_at);
    }

    #[allow(clippy::too_many_lines)]
    fn send_message(&mut self, message: &OutboundMessage) -> Result<SendReceipt, AdapterFailure> {
        self.ensure_active()?;
        if message.route.channel != "email"
            || message.route.external_account != self.config.channel_account
            || message.idempotency_key.trim().is_empty()
        {
            return Err(permanent(
                "email delivery route belongs to another adapter account",
            ));
        }
        if let Some(EmailDeliveryState::Accepted {
            provider_message_id,
        }) = self.cursor.deliveries.get(&message.idempotency_key)
        {
            return Ok(SendReceipt {
                platform_message_id: provider_message_id.clone(),
                accepted_at: now(),
                duplicate_possible: false,
            });
        }
        let request = self.prepare_send(message)?;
        self.cursor
            .deliveries
            .insert(message.idempotency_key.clone(), EmailDeliveryState::Pending);
        let response = self.access_token.with_bytes(|token| {
            let token = std::str::from_utf8(token)
                .map_err(|_| permanent("email provider token is not UTF-8"))?;
            self.http
                .post(&request.url)
                .header("Authorization", format!("Bearer {token}"))
                .header("Content-Type", &request.content_type)
                .header("Idempotency-Key", &message.idempotency_key)
                .send(&request.body)
                .map_err(|error| transport_error("email provider", &error))
        });
        let mut response = match response {
            Ok(response) => response,
            Err(error) => {
                self.cursor.deliveries.insert(
                    message.idempotency_key.clone(),
                    EmailDeliveryState::PossibleDuplicate,
                );
                self.safe_error = Some("email acknowledgement is uncertain".to_owned());
                return Err(error);
            }
        };
        let status = response.status().as_u16();
        let response_body = response
            .body_mut()
            .with_config()
            .limit(u64::try_from(self.config.max_event_bytes).unwrap_or(u64::MAX))
            .read_to_vec()
            .map_err(|error| transport_error("email provider", &error));
        let response_body = match response_body {
            Ok(response_body) => response_body,
            Err(error) => {
                self.cursor.deliveries.insert(
                    message.idempotency_key.clone(),
                    EmailDeliveryState::PossibleDuplicate,
                );
                self.safe_error = Some("email acknowledgement is uncertain".to_owned());
                return Err(error);
            }
        };
        match status {
            200..=299 => {
                let Ok(receipt) = serde_json::from_slice::<EmailProviderReceipt>(&response_body)
                else {
                    self.cursor.deliveries.insert(
                        message.idempotency_key.clone(),
                        EmailDeliveryState::PossibleDuplicate,
                    );
                    self.safe_error = Some("email acknowledgement is uncertain".to_owned());
                    return Err(permanent("email provider acknowledgement is uncertain"));
                };
                self.cursor.deliveries.insert(
                    message.idempotency_key.clone(),
                    EmailDeliveryState::Accepted {
                        provider_message_id: receipt.message_id.clone(),
                    },
                );
                for id in &message.artifacts {
                    if let Some(upload) = self.staged.remove(id) {
                        self.staged_bytes = self.staged_bytes.saturating_sub(upload.bytes.len());
                    }
                }
                self.safe_error = None;
                Ok(SendReceipt {
                    platform_message_id: receipt.message_id,
                    accepted_at: now(),
                    duplicate_possible: !self.config.provider_guarantees_idempotency,
                })
            }
            401 | 403 => {
                self.cursor.deliveries.insert(
                    message.idempotency_key.clone(),
                    EmailDeliveryState::Rejected,
                );
                Err(permanent(
                    "email provider authentication or permission denied",
                ))
            }
            429 => Err(rate_limited("email provider rate limit reached", None)),
            500..=599 => {
                self.cursor.deliveries.insert(
                    message.idempotency_key.clone(),
                    EmailDeliveryState::PossibleDuplicate,
                );
                self.safe_error = Some("email acknowledgement is uncertain".to_owned());
                Err(retryable("email provider acknowledgement is uncertain"))
            }
            _ => {
                self.cursor.deliveries.insert(
                    message.idempotency_key.clone(),
                    EmailDeliveryState::Rejected,
                );
                Err(permanent("email provider rejected the message"))
            }
        }
    }
}

impl ChannelAdapterV2 for EmailAdapter {
    fn capabilities_v2(&self) -> ChannelCapabilitiesV2 {
        email_capabilities(&self.config)
    }

    fn receive_v2(&mut self) -> Result<ChannelEventV2, ChannelAdapterErrorV2> {
        self.ensure_active().map_err(ChannelAdapterErrorV2::from)?;
        self.inbound.pop_front().ok_or_else(|| {
            v2_error(
                ChannelAdapterErrorKindV2::TransientNetwork,
                "email has no pending verified provider event",
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
                        "email rich HTML content is not enabled by this adapter",
                    ));
                }
                if self.cancelled.contains(&message.idempotency_key) {
                    return Err(v2_error(
                        ChannelAdapterErrorKindV2::Cancelled,
                        "email delivery was cancelled before dispatch",
                    ));
                }
                let receipt = self
                    .send_message(&OutboundMessage {
                        route: message.route.clone(),
                        idempotency_key: message.idempotency_key.clone(),
                        text: message.text.clone(),
                        artifacts: message.artifacts.clone(),
                    })
                    .map_err(|failure| {
                        let possible_duplicate = matches!(
                            self.cursor.deliveries.get(&message.idempotency_key),
                            Some(EmailDeliveryState::PossibleDuplicate)
                        );
                        if possible_duplicate {
                            v2_error(
                                ChannelAdapterErrorKindV2::UncertainAcknowledgement,
                                "email provider acknowledgement is uncertain",
                            )
                        } else {
                            ChannelAdapterErrorV2::from(failure)
                        }
                    })?;
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
                        "email cancellation identity is empty",
                    ));
                }
                if matches!(
                    self.cursor.deliveries.get(cancellation_id),
                    Some(EmailDeliveryState::Accepted { .. })
                ) {
                    return Err(ChannelAdapterErrorV2::unsupported(
                        "accepted email cannot be recalled",
                    ));
                }
                self.cancelled.insert(cancellation_id.clone());
                self.cursor
                    .deliveries
                    .insert(cancellation_id.clone(), EmailDeliveryState::Cancelled);
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
                "email operation is unsupported",
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
            .recent_message_ids
            .last()
            .zip(self.cursor.last_event_at)
            .map(|(value, observed_at)| ReconnectCursorV2 {
                value: value.clone(),
                observed_at,
            })
    }
}

impl ChannelAdapter for EmailAdapter {
    fn features(&self) -> AdapterFeatures {
        let mut capabilities = BTreeSet::from([
            AdapterCapability::Attachments,
            AdapterCapability::Threads,
            AdapterCapability::Reconnect,
        ]);
        if self.config.provider_guarantees_idempotency {
            capabilities.insert(AdapterCapability::IdempotentSend);
        }
        AdapterFeatures {
            capabilities,
            max_attachment_bytes: self.config.max_attachment_bytes,
            requests_per_minute: None,
        }
    }

    fn receive(&mut self) -> Result<AdapterEvent, AdapterFailure> {
        loop {
            let event = self.receive_v2().map_err(adapter_failure_from_v2)?;
            if let Some(event) = event_v2_to_v1("email", event) {
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
struct EmailProviderMessage {
    message_id: String,
    from: String,
    to: Vec<String>,
    subject: String,
    text_body: Option<String>,
    html_body: Option<String>,
    in_reply_to: Option<String>,
    #[serde(default)]
    references: Vec<String>,
    received_at: Option<String>,
    #[serde(default)]
    attachments: Vec<EmailProviderAttachment>,
}

#[derive(Debug, Deserialize)]
struct EmailProviderAttachment {
    id: String,
    file_name: String,
    media_type: String,
    byte_length: u64,
    download_url: Option<String>,
}

#[derive(Debug, Deserialize)]
struct EmailProviderReceipt {
    #[serde(alias = "id")]
    message_id: String,
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct EmailProviderAccount {
    sender_address: String,
}

fn normalize_attachments(
    values: Vec<EmailProviderAttachment>,
    max_attachment_bytes: u64,
) -> Result<Vec<ChannelAttachmentV2>, AdapterFailure> {
    values
        .into_iter()
        .map(|value| {
            if value.id.trim().is_empty()
                || value.file_name.trim().is_empty()
                || value.media_type.trim().is_empty()
                || value.byte_length > max_attachment_bytes
            {
                return Err(permanent("email attachment is malformed or oversized"));
            }
            let kind = if value.media_type.starts_with("image/") {
                ChannelAttachmentKindV2::Image
            } else if value.media_type.starts_with("audio/") {
                ChannelAttachmentKindV2::Audio
            } else if value.media_type.starts_with("video/") {
                ChannelAttachmentKindV2::Video
            } else {
                ChannelAttachmentKindV2::File
            };
            Ok(ChannelAttachmentV2 {
                attachment: Attachment {
                    id: value.id,
                    file_name: value.file_name,
                    media_type: value.media_type,
                    byte_length: value.byte_length,
                    artifact_id: None,
                    download_url: value.download_url,
                    staging_file: None,
                    sha256: None,
                },
                kind,
                duration_ms: None,
                metadata: BTreeMap::new(),
            })
        })
        .collect()
}

fn normalize_message_id(value: &str) -> Option<String> {
    let value = value.trim();
    if value.len() < 3 || value.contains(['\r', '\n']) {
        return None;
    }
    if value.starts_with('<') && value.ends_with('>') {
        Some(value.to_owned())
    } else if value.contains('@') && !value.contains(char::is_whitespace) {
        Some(format!("<{value}>"))
    } else {
        None
    }
}

fn valid_email_address(value: &str) -> bool {
    let value = value.trim();
    let mut parts = value.split('@');
    let local = parts.next().unwrap_or_default();
    let domain = parts.next().unwrap_or_default();
    !local.is_empty()
        && !domain.is_empty()
        && domain.contains('.')
        && parts.next().is_none()
        && !value.contains(['\r', '\n', ' ', '<', '>'])
}

fn valid_relative_api_path(value: &str) -> bool {
    value.starts_with('/')
        && !value.starts_with("//")
        && !value.contains("..")
        && !value.contains(['?', '#'])
}

fn reply_subject(subject: &str) -> String {
    if subject
        .get(..3)
        .is_some_and(|prefix| prefix.eq_ignore_ascii_case("re:"))
    {
        subject.to_owned()
    } else {
        format!("Re: {subject}")
    }
}

fn base64_encode(bytes: &[u8]) -> String {
    const ALPHABET: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut output = String::with_capacity(bytes.len().div_ceil(3) * 4);
    for chunk in bytes.chunks(3) {
        let first = chunk[0];
        let second = chunk.get(1).copied().unwrap_or(0);
        let third = chunk.get(2).copied().unwrap_or(0);
        output.push(char::from(ALPHABET[usize::from(first >> 2)]));
        output.push(char::from(
            ALPHABET[usize::from(((first & 0x03) << 4) | (second >> 4))],
        ));
        if chunk.len() > 1 {
            output.push(char::from(
                ALPHABET[usize::from(((second & 0x0f) << 2) | (third >> 6))],
            ));
        } else {
            output.push('=');
        }
        if chunk.len() > 2 {
            output.push(char::from(ALPHABET[usize::from(third & 0x3f)]));
        } else {
            output.push('=');
        }
    }
    output
}

fn email_capabilities(config: &EmailConfig) -> ChannelCapabilitiesV2 {
    let unsupported = |reason: &str| ChannelCapabilitySupportV2::Unsupported {
        safe_reason: reason.to_owned(),
    };
    let mut declarations = ChannelCapabilityV2::ALL
        .into_iter()
        .map(|capability| {
            (
                capability,
                unsupported("email capability is not enabled by this adapter"),
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
    if config.provider_guarantees_idempotency {
        declarations.insert(
            ChannelCapabilityV2::IdempotentSend,
            ChannelCapabilitySupportV2::Supported,
        );
    } else {
        declarations.insert(
            ChannelCapabilityV2::IdempotentSend,
            unsupported("email provider does not guarantee idempotent send"),
        );
    }
    declarations.insert(
        ChannelCapabilityV2::Mentions,
        unsupported("email has recipients rather than message mentions"),
    );
    declarations.insert(
        ChannelCapabilityV2::Commands,
        unsupported("email commands are not interpreted by the adapter"),
    );
    declarations.insert(
        ChannelCapabilityV2::MessageEdits,
        unsupported("sent email cannot be edited"),
    );
    declarations.insert(
        ChannelCapabilityV2::MessageDeletion,
        unsupported("sent email cannot be remotely deleted"),
    );
    declarations.insert(
        ChannelCapabilityV2::Reactions,
        unsupported("email does not have portable message reactions"),
    );
    declarations.insert(
        ChannelCapabilityV2::Voice,
        unsupported("email audio is represented as an attachment"),
    );
    declarations.insert(
        ChannelCapabilityV2::RichContent,
        unsupported("HTML email is not enabled by this adapter"),
    );
    declarations.insert(
        ChannelCapabilityV2::Typing,
        unsupported("email does not expose typing state"),
    );
    declarations.insert(
        ChannelCapabilityV2::DeliveryReceipts,
        unsupported("portable provider delivery webhooks are not enabled here"),
    );
    declarations.insert(
        ChannelCapabilityV2::ReadReceipts,
        unsupported("email read receipts are not reliable or portable"),
    );
    ChannelCapabilitiesV2 {
        contract: ChannelContractVersion::new(2, 0),
        declarations,
        max_event_bytes: u64::try_from(config.max_event_bytes).unwrap_or(u64::MAX),
        max_attachment_bytes: config.max_attachment_bytes,
        max_attachments: 20,
        max_rich_content_bytes: u64::try_from(config.max_event_bytes).unwrap_or(u64::MAX),
        requests_per_minute: None,
    }
}
