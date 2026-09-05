use std::collections::{BTreeMap, BTreeSet, VecDeque};
use std::io::Write as _;
use std::time::Duration;

use hmac::{Hmac, Mac};
use keith_agent_types::{ArtifactId, UtcTimestamp};
use keith_channel_core::{
    AdapterCapability, AdapterEvent, AdapterFailure, AdapterFeatures, Attachment,
    CHANNEL_CONTRACT_V2, ChannelAccountSetupV2, ChannelAdapter, ChannelAdapterErrorKindV2,
    ChannelAdapterErrorV2, ChannelAdapterV2, ChannelAttachmentKindV2, ChannelAttachmentV2,
    ChannelCapabilitiesV2, ChannelCapabilitySupportV2, ChannelCapabilityV2, ChannelCommandV2,
    ChannelConnectionHealthV2, ChannelContractVersion, ChannelConversationKindV2,
    ChannelConversationV2, ChannelEventKindV2, ChannelEventV2, ChannelIdentityV2, ChannelMessageV2,
    ChannelOperationReceiptV2, ChannelOperationV2, ChannelOutboundMessageV2, ChannelReceiptStateV2,
    InboundIntent, InboundMessage, OutboundMessage, ReconnectCursorV2, ReplyRoute, RetryClass,
    SendReceipt,
};
use keith_credentials::SecretValue;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use sha2::Sha256;

const WHATSAPP_CHANNEL: &str = "whatsapp_cloud";

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WhatsAppCloudConfig {
    pub api_base: String,
    pub graph_version: String,
    pub business_account_id: String,
    pub phone_number_id: String,
    pub max_event_bytes: usize,
    pub max_attachment_bytes: u64,
    pub max_staged_bytes: usize,
    pub timeout_ms: u64,
    pub deduplication_capacity: usize,
    pub max_pending_events: usize,
}

impl WhatsAppCloudConfig {
    pub fn production(
        graph_version: impl Into<String>,
        business_account_id: impl Into<String>,
        phone_number_id: impl Into<String>,
    ) -> Self {
        Self {
            api_base: "https://graph.facebook.com".to_owned(),
            graph_version: graph_version.into(),
            business_account_id: business_account_id.into(),
            phone_number_id: phone_number_id.into(),
            max_event_bytes: 1024 * 1_024,
            max_attachment_bytes: 16 * 1_024 * 1_024,
            max_staged_bytes: 32 * 1_024 * 1_024,
            timeout_ms: 30_000,
            deduplication_capacity: 4_096,
            max_pending_events: 1_024,
        }
    }
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct WhatsAppCursor {
    pub recent_message_ids: Vec<String>,
    pub recent_status_keys: Vec<String>,
    #[serde(default)]
    pub last_event_at: Option<UtcTimestamp>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum WhatsAppDeliveryState {
    Sent,
    Delivered,
    Read,
    Failed,
    Deleted,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct WhatsAppDeliveryStatus {
    pub account_id: String,
    pub message_id: String,
    pub recipient: String,
    pub state: WhatsAppDeliveryState,
    pub occurred_at: UtcTimestamp,
    pub safe_error_codes: Vec<u64>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum WhatsAppUploadKind {
    Audio,
    Document,
    Image,
    Sticker,
    Video,
}

impl WhatsAppUploadKind {
    const fn api_name(self) -> &'static str {
        match self {
            Self::Audio => "audio",
            Self::Document => "document",
            Self::Image => "image",
            Self::Sticker => "sticker",
            Self::Video => "video",
        }
    }

    const fn supports_caption(self) -> bool {
        matches!(self, Self::Document | Self::Image | Self::Video)
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WhatsAppUpload {
    pub kind: WhatsAppUploadKind,
    pub file_name: String,
    pub media_type: String,
    pub bytes: Vec<u8>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WhatsAppDownload {
    pub media_type: String,
    pub sha256: Option<String>,
    pub bytes: Vec<u8>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WhatsAppTemplate {
    pub name: String,
    pub language_code: String,
    pub components: Vec<Value>,
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct WhatsAppWebhookOutcome {
    pub messages: usize,
    pub statuses: usize,
    pub duplicates: usize,
    pub ignored: usize,
}

pub struct WhatsAppCloudAdapter {
    config: WhatsAppCloudConfig,
    access_token: SecretValue,
    app_secret: SecretValue,
    verify_token: SecretValue,
    http: ureq::Agent,
    cursor: WhatsAppCursor,
    seen_messages: BTreeSet<String>,
    message_order: VecDeque<String>,
    seen_statuses: BTreeSet<String>,
    status_order: VecDeque<String>,
    pending: VecDeque<AdapterEvent>,
    pending_v2: VecDeque<ChannelEventV2>,
    statuses: VecDeque<WhatsAppDeliveryStatus>,
    staged: BTreeMap<ArtifactId, WhatsAppUpload>,
    staged_bytes: usize,
    cancelled: BTreeSet<String>,
}

impl WhatsAppCloudAdapter {
    /// Creates a Cloud API adapter with profile-owned credentials and durable deduplication state.
    ///
    /// # Errors
    ///
    /// Returns a permanent failure for unsafe endpoints, malformed account identifiers, or zero
    /// resource bounds.
    pub fn new(
        mut config: WhatsAppCloudConfig,
        access_token: SecretValue,
        app_secret: SecretValue,
        verify_token: SecretValue,
        cursor: WhatsAppCursor,
    ) -> Result<Self, AdapterFailure> {
        let api_base_len = config.api_base.trim_end_matches('/').len();
        config.api_base.truncate(api_base_len);
        let endpoint_valid = config.api_base.starts_with("https://")
            || config.api_base.starts_with("http://127.0.0.1:")
            || config.api_base.starts_with("http://localhost:");
        let version_valid = valid_graph_version(&config.graph_version);
        if !endpoint_valid
            || !version_valid
            || !safe_graph_identifier(&config.business_account_id)
            || !safe_graph_identifier(&config.phone_number_id)
            || config.max_event_bytes == 0
            || config.max_attachment_bytes == 0
            || config.max_staged_bytes == 0
            || config.timeout_ms == 0
            || config.deduplication_capacity == 0
            || config.max_pending_events == 0
        {
            return Err(permanent("invalid WhatsApp Cloud adapter configuration"));
        }
        let message_order =
            bounded_order(&cursor.recent_message_ids, config.deduplication_capacity);
        let status_order = bounded_order(&cursor.recent_status_keys, config.deduplication_capacity);
        let seen_messages = message_order.iter().cloned().collect();
        let seen_statuses = status_order.iter().cloned().collect();
        let http: ureq::Agent = ureq::Agent::config_builder()
            .timeout_global(Some(Duration::from_millis(config.timeout_ms)))
            .http_status_as_error(false)
            .build()
            .into();
        let adapter = Self {
            config,
            access_token,
            app_secret,
            verify_token,
            http,
            cursor,
            seen_messages,
            message_order,
            seen_statuses,
            status_order,
            pending: VecDeque::new(),
            pending_v2: VecDeque::new(),
            statuses: VecDeque::new(),
            staged: BTreeMap::new(),
            staged_bytes: 0,
            cancelled: BTreeSet::new(),
        };
        adapter
            .capabilities_v2()
            .validate()
            .map_err(v2_to_adapter_failure)?;
        Ok(adapter)
    }

    pub const fn cursor(&self) -> &WhatsAppCursor {
        &self.cursor
    }

    pub fn account_setup_v2(&self) -> ChannelAccountSetupV2 {
        ChannelAccountSetupV2 {
            account_id: self.config.phone_number_id.clone(),
            required_credential_names: BTreeSet::from([
                "access_token".to_owned(),
                "app_secret".to_owned(),
                "verify_token".to_owned(),
            ]),
            required_scopes: BTreeSet::from(["whatsapp_business_messaging".to_owned()]),
            webhook_configured: true,
            socket_or_polling_configured: false,
            connection_health: ChannelConnectionHealthV2::Disconnected,
            reconnect_cursor_present: !self.cursor.recent_message_ids.is_empty()
                || !self.cursor.recent_status_keys.is_empty(),
            safe_test_supported: true,
            metadata: BTreeMap::from([
                (
                    "business_account_id".to_owned(),
                    self.config.business_account_id.clone(),
                ),
                (
                    "graph_version".to_owned(),
                    self.config.graph_version.clone(),
                ),
            ]),
        }
    }

    /// Runs the read-only phone-number lookup and verifies account continuity.
    ///
    /// # Errors
    ///
    /// Returns a classified error for transport, authentication, malformed data, or mismatch.
    pub fn test_connection(&self) -> Result<(), ChannelAdapterErrorV2> {
        let result = self
            .graph_get(&self.config.phone_number_id)
            .map_err(adapter_failure_to_v2)?;
        if result.get("id").and_then(Value::as_str) == Some(&self.config.phone_number_id) {
            Ok(())
        } else {
            Err(v2_error(
                ChannelAdapterErrorKindV2::Permission,
                "WhatsApp credential belongs to another phone number",
            ))
        }
    }

    /// Replaces the Cloud API bearer credential in memory. The old token is zeroed on drop.
    pub fn rotate_access_token(&mut self, access_token: SecretValue) {
        self.access_token = access_token;
    }

    /// Validates Meta's webhook subscription challenge without exposing the configured token.
    pub fn verify_challenge<'a>(
        &self,
        mode: &str,
        presented_verify_token: &[u8],
        challenge: &'a str,
    ) -> Option<&'a str> {
        let valid = mode == "subscribe"
            && !challenge.is_empty()
            && self
                .verify_token
                .with_bytes(|expected| secret_matches(expected, presented_verify_token));
        valid.then_some(challenge)
    }

    /// Verifies `X-Hub-Signature-256` over the raw body before parsing any untrusted JSON.
    ///
    /// # Errors
    ///
    /// Returns a permanent failure for an invalid signature, oversized body, malformed payload,
    /// or a webhook belonging to another `WhatsApp` account.
    pub fn ingest_webhook(
        &mut self,
        signature_header: &str,
        body: &[u8],
    ) -> Result<WhatsAppWebhookOutcome, AdapterFailure> {
        if body.len() > self.config.max_event_bytes {
            return Err(permanent("WhatsApp webhook body exceeded its bound"));
        }
        let signature = decode_signature(signature_header)
            .ok_or_else(|| authentication("WhatsApp webhook signature is malformed"))?;
        let authentic = self
            .app_secret
            .with_bytes(|secret| constant_time_eq(&hmac_sha256(secret, body), &signature));
        if !authentic {
            return Err(authentication("WhatsApp webhook authentication failed"));
        }
        let payload = serde_json::from_slice::<Value>(body)
            .map_err(|_| malformed("WhatsApp webhook body is malformed"))?;
        self.normalize_webhook(&payload)
    }

    /// Returns the next delivery/read/failure projection received through the verified webhook.
    pub fn take_status(&mut self) -> Option<WhatsAppDeliveryStatus> {
        self.statuses.pop_front()
    }

    /// Stages a bounded media object for a later Cloud API upload and send.
    ///
    /// # Errors
    ///
    /// Returns a permanent failure for unsafe metadata or an exceeded byte budget.
    pub fn stage_artifact(
        &mut self,
        id: ArtifactId,
        upload: WhatsAppUpload,
    ) -> Result<(), AdapterFailure> {
        if upload.file_name.trim().is_empty()
            || upload.media_type.trim().is_empty()
            || upload.file_name.contains(['\r', '\n', '"'])
            || upload.media_type.contains(['\r', '\n'])
            || upload.bytes.is_empty()
            || u64::try_from(upload.bytes.len()).unwrap_or(u64::MAX)
                > self.config.max_attachment_bytes
        {
            return Err(permanent("WhatsApp media is invalid or oversized"));
        }
        let old = self.staged.get(&id).map_or(0, |value| value.bytes.len());
        let next = self
            .staged_bytes
            .saturating_sub(old)
            .saturating_add(upload.bytes.len());
        if next > self.config.max_staged_bytes {
            return Err(permanent("WhatsApp staged media budget exceeded"));
        }
        self.staged.insert(id, upload);
        self.staged_bytes = next;
        Ok(())
    }

    /// Resolves and downloads media from Meta while keeping bearer URLs out of normalized events.
    ///
    /// # Errors
    ///
    /// Returns a classified Graph API, transport, URL, or media-bound failure.
    pub fn download_media(&self, media_id: &str) -> Result<WhatsAppDownload, AdapterFailure> {
        if !safe_graph_identifier(media_id) {
            return Err(malformed("WhatsApp media identity is unsafe"));
        }
        let metadata = self.graph_get(media_id)?;
        let url = metadata
            .get("url")
            .and_then(Value::as_str)
            .filter(|url| safe_media_url(url))
            .ok_or_else(|| malformed("WhatsApp returned an unsafe media URL"))?;
        let media_type = metadata
            .get("mime_type")
            .and_then(Value::as_str)
            .unwrap_or("application/octet-stream")
            .to_owned();
        let declared_size = metadata
            .get("file_size")
            .and_then(Value::as_u64)
            .unwrap_or(0);
        if declared_size > self.config.max_attachment_bytes {
            return Err(permanent("WhatsApp media exceeded its declared bound"));
        }
        let mut response = self.authorized_get(url)?;
        let bytes = response
            .body_mut()
            .with_config()
            .limit(self.config.max_attachment_bytes.saturating_add(1))
            .read_to_vec()
            .map_err(|error| transport_failure(&error))?;
        if bytes.is_empty()
            || u64::try_from(bytes.len()).unwrap_or(u64::MAX) > self.config.max_attachment_bytes
        {
            return Err(permanent("WhatsApp downloaded media exceeded its bound"));
        }
        Ok(WhatsAppDownload {
            media_type,
            sha256: metadata
                .get("sha256")
                .and_then(Value::as_str)
                .map(str::to_owned),
            bytes,
        })
    }

    /// Sends an approved `WhatsApp` template to an adapter-owned destination.
    ///
    /// # Errors
    ///
    /// Returns a classified validation, permission, rate-limit, or Graph API failure.
    pub fn send_template(
        &self,
        route: &ReplyRoute,
        template: &WhatsAppTemplate,
    ) -> Result<SendReceipt, AdapterFailure> {
        self.validate_route(route)?;
        if template.name.trim().is_empty()
            || template.language_code.trim().is_empty()
            || template.name.len() > 512
            || template.language_code.len() > 35
        {
            return Err(malformed("WhatsApp template identity is malformed"));
        }
        let component_bytes = serde_json::to_vec(&template.components)
            .map_err(|_| malformed("WhatsApp template components are malformed"))?;
        if component_bytes.len() > self.config.max_event_bytes {
            return Err(permanent(
                "WhatsApp template components exceeded their byte bound",
            ));
        }
        let result = self.graph_post(
            &format!("{}/messages", self.config.phone_number_id),
            &json!({
                "messaging_product": "whatsapp",
                "recipient_type": "individual",
                "to": route.conversation,
                "type": "template",
                "template": {
                    "name": template.name,
                    "language": {"code": template.language_code},
                    "components": template.components,
                }
            }),
        )?;
        send_receipt(&result)
    }

    /// Marks an inbound `WhatsApp` message read through the Cloud API.
    ///
    /// # Errors
    ///
    /// Returns a classified identity or Graph API failure.
    pub fn mark_read(&self, message_id: &str) -> Result<(), AdapterFailure> {
        if message_id.trim().is_empty() {
            return Err(malformed("WhatsApp message identity is empty"));
        }
        self.graph_post(
            &format!("{}/messages", self.config.phone_number_id),
            &json!({
                "messaging_product": "whatsapp",
                "status": "read",
                "message_id": message_id,
            }),
        )?;
        Ok(())
    }

    fn normalize_webhook(
        &mut self,
        payload: &Value,
    ) -> Result<WhatsAppWebhookOutcome, AdapterFailure> {
        if payload.get("object").and_then(Value::as_str) != Some("whatsapp_business_account") {
            return Err(malformed("WhatsApp webhook object is unsupported"));
        }
        let entries = payload
            .get("entry")
            .and_then(Value::as_array)
            .ok_or_else(|| malformed("WhatsApp webhook has no entries"))?;
        let mut outcome = WhatsAppWebhookOutcome::default();
        for entry in entries {
            if string_field(entry, "id")? != self.config.business_account_id {
                return Err(permission(
                    "WhatsApp webhook belongs to another business account",
                ));
            }
            let changes = entry
                .get("changes")
                .and_then(Value::as_array)
                .ok_or_else(|| malformed("WhatsApp webhook has no changes"))?;
            for change in changes {
                if change.get("field").and_then(Value::as_str) != Some("messages") {
                    outcome.ignored = outcome.ignored.saturating_add(1);
                    continue;
                }
                let value = change
                    .get("value")
                    .ok_or_else(|| malformed("WhatsApp change has no value"))?;
                self.validate_webhook_account(value)?;
                if let Some(messages) = value.get("messages").and_then(Value::as_array) {
                    for message in messages {
                        let message_id = string_field(message, "id")?;
                        if self.seen_messages.contains(&message_id) {
                            outcome.duplicates = outcome.duplicates.saturating_add(1);
                            continue;
                        }
                        match self.normalize_message(message)? {
                            (Some(event), Some(event_v2)) => {
                                if self.pending.len() >= self.config.max_pending_events
                                    || self.pending_v2.len() >= self.config.max_pending_events
                                {
                                    return Err(retryable(
                                        "WhatsApp inbound queue reached its bound",
                                    ));
                                }
                                event_v2
                                    .validate(&self.capabilities_v2())
                                    .map_err(v2_to_adapter_failure)?;
                                self.pending.push_back(event);
                                self.cursor.last_event_at =
                                    event_occurred_at(&event_v2).or_else(|| Some(now()));
                                self.pending_v2.push_back(event_v2);
                                outcome.messages = outcome.messages.saturating_add(1);
                            }
                            (None, None) => outcome.ignored = outcome.ignored.saturating_add(1),
                            _ => {
                                return Err(malformed(
                                    "WhatsApp normalization produced inconsistent events",
                                ));
                            }
                        }
                        self.remember_message(message_id);
                    }
                }
                if let Some(statuses) = value.get("statuses").and_then(Value::as_array) {
                    for status in statuses {
                        let normalized = normalize_status(status, &self.config.phone_number_id)?;
                        let key = format!(
                            "{}\0{:?}\0{}",
                            normalized.message_id,
                            normalized.state,
                            normalized.occurred_at.unix_millis()
                        );
                        if self.seen_statuses.contains(&key) {
                            outcome.duplicates = outcome.duplicates.saturating_add(1);
                            continue;
                        }
                        if self.statuses.len() >= self.config.max_pending_events
                            || self.pending_v2.len() >= self.config.max_pending_events
                        {
                            return Err(retryable("WhatsApp status queue reached its bound"));
                        }
                        let event_v2 = whatsapp_status_event_v2(&normalized);
                        event_v2
                            .validate(&self.capabilities_v2())
                            .map_err(v2_to_adapter_failure)?;
                        self.cursor.last_event_at = Some(normalized.occurred_at);
                        self.pending_v2.push_back(event_v2);
                        self.statuses.push_back(normalized);
                        self.remember_status(key);
                        outcome.statuses = outcome.statuses.saturating_add(1);
                    }
                }
            }
        }
        Ok(outcome)
    }

    fn validate_webhook_account(&self, value: &Value) -> Result<(), AdapterFailure> {
        if value.get("messaging_product").and_then(Value::as_str) != Some("whatsapp") {
            return Err(malformed("WhatsApp webhook product is unsupported"));
        }
        let phone_number_id = value
            .get("metadata")
            .and_then(|metadata| metadata.get("phone_number_id"))
            .and_then(Value::as_str)
            .ok_or_else(|| malformed("WhatsApp webhook has no phone number identity"))?;
        if phone_number_id != self.config.phone_number_id {
            return Err(permission(
                "WhatsApp webhook belongs to another phone number",
            ));
        }
        Ok(())
    }

    fn normalize_message(
        &self,
        message: &Value,
    ) -> Result<(Option<AdapterEvent>, Option<ChannelEventV2>), AdapterFailure> {
        let message_type = string_field(message, "type")?;
        let (text, attachments) = match message_type.as_str() {
            "text" => (
                message
                    .pointer("/text/body")
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_owned(),
                Vec::new(),
            ),
            "button" => (
                message
                    .pointer("/button/text")
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_owned(),
                Vec::new(),
            ),
            "interactive" => (
                message
                    .pointer("/interactive/button_reply/title")
                    .or_else(|| message.pointer("/interactive/list_reply/title"))
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_owned(),
                Vec::new(),
            ),
            "audio" | "document" | "image" | "sticker" | "video" => {
                let media = message
                    .get(&message_type)
                    .ok_or_else(|| malformed("WhatsApp media payload is missing"))?;
                let caption = media
                    .get("caption")
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_owned();
                (
                    caption,
                    vec![whatsapp_attachment(
                        media,
                        &message_type,
                        self.config.max_attachment_bytes,
                    )?],
                )
            }
            _ => return Ok((None, None)),
        };
        if text.trim().is_empty() && attachments.is_empty() {
            return Ok((None, None));
        }
        let occurred_at = message
            .get("timestamp")
            .and_then(Value::as_str)
            .and_then(|timestamp| timestamp.parse::<i64>().ok())
            .and_then(|seconds| seconds.checked_mul(1_000))
            .map_or_else(now, UtcTimestamp::from_unix_millis);
        let inbound = InboundMessage {
            channel: WHATSAPP_CHANNEL.to_owned(),
            external_account: self.config.phone_number_id.clone(),
            conversation: string_field(message, "from")?,
            thread: None,
            sender: string_field(message, "from")?,
            message_id: string_field(message, "id")?,
            reply_target: message
                .pointer("/context/id")
                .and_then(Value::as_str)
                .map(str::to_owned),
            intent: command_intent(&text),
            text,
            attachments,
            occurred_at,
        };
        let event_v2 = whatsapp_message_event_v2(message, &inbound);
        Ok((
            Some(AdapterEvent::Inbound(Box::new(inbound))),
            Some(event_v2),
        ))
    }

    fn remember_message(&mut self, message_id: String) {
        remember(
            message_id,
            &mut self.seen_messages,
            &mut self.message_order,
            self.config.deduplication_capacity,
        );
        self.cursor.recent_message_ids = self.message_order.iter().cloned().collect();
    }

    fn remember_status(&mut self, status_key: String) {
        remember(
            status_key,
            &mut self.seen_statuses,
            &mut self.status_order,
            self.config.deduplication_capacity,
        );
        self.cursor.recent_status_keys = self.status_order.iter().cloned().collect();
    }

    fn validate_route(&self, route: &ReplyRoute) -> Result<(), AdapterFailure> {
        if route.channel != WHATSAPP_CHANNEL
            || route.external_account != self.config.phone_number_id
            || route.conversation.trim().is_empty()
            || route.thread.is_some()
        {
            return Err(permission(
                "WhatsApp route does not belong to this adapter account",
            ));
        }
        Ok(())
    }

    fn send_message(&mut self, message: &OutboundMessage) -> Result<SendReceipt, AdapterFailure> {
        self.validate_route(&message.route)?;
        if message.artifacts.len() > 1 {
            return Err(unsupported(
                "WhatsApp adapter supports one staged media item per outbox entry",
            ));
        }
        let mut payload = if let Some(artifact_id) = message.artifacts.first() {
            let upload = self
                .staged
                .get(artifact_id)
                .ok_or_else(|| malformed("WhatsApp media bytes were not staged"))?;
            if upload.kind.supports_caption() && !message.text.is_empty() {
                if message.text.chars().count() > 1_024 {
                    return Err(permanent("WhatsApp media caption exceeds 1024 characters"));
                }
            } else if !message.text.is_empty() {
                return Err(unsupported(
                    "WhatsApp audio and sticker sends do not support a caption",
                ));
            }
            let media_id = self.upload_media(upload)?;
            let kind = upload.kind.api_name();
            let mut media = json!({"id": media_id});
            if upload.kind == WhatsAppUploadKind::Document {
                media["filename"] = json!(upload.file_name);
            }
            if upload.kind.supports_caption() && !message.text.is_empty() {
                media["caption"] = json!(message.text);
            }
            let mut payload = json!({
                "messaging_product": "whatsapp",
                "recipient_type": "individual",
                "to": message.route.conversation,
                "type": kind,
            });
            payload[kind] = media;
            payload
        } else {
            if message.text.is_empty() || message.text.chars().count() > 4_096 {
                return Err(permanent("WhatsApp message text exceeds its bound"));
            }
            json!({
                "messaging_product": "whatsapp",
                "recipient_type": "individual",
                "to": message.route.conversation,
                "type": "text",
                "text": {"preview_url": false, "body": message.text},
            })
        };
        if let Some(reply) = &message.route.reply_to_message {
            payload["context"] = json!({"message_id": reply});
        }
        let result = self.graph_post(
            &format!("{}/messages", self.config.phone_number_id),
            &payload,
        )?;
        for artifact in &message.artifacts {
            if let Some(removed) = self.staged.remove(artifact) {
                self.staged_bytes = self.staged_bytes.saturating_sub(removed.bytes.len());
            }
        }
        send_receipt(&result)
    }

    fn upload_media(&self, upload: &WhatsAppUpload) -> Result<String, AdapterFailure> {
        let boundary = "keith-whatsapp-boundary";
        let mut body = Vec::new();
        multipart_text(&mut body, boundary, "messaging_product", "whatsapp")?;
        multipart_text(&mut body, boundary, "type", &upload.media_type)?;
        write!(
            body,
            "--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"{}\"\r\nContent-Type: {}\r\n\r\n",
            upload.file_name, upload.media_type
        )
        .map_err(|_| malformed("WhatsApp multipart payload could not be encoded"))?;
        body.extend_from_slice(&upload.bytes);
        body.extend_from_slice(b"\r\n");
        write!(body, "--{boundary}--\r\n")
            .map_err(|_| malformed("WhatsApp multipart payload could not be encoded"))?;
        let result = self.graph_post_bytes(
            &format!("{}/media", self.config.phone_number_id),
            &body,
            &format!("multipart/form-data; boundary={boundary}"),
        )?;
        string_field(&result, "id")
    }

    fn graph_get(&self, path: &str) -> Result<Value, AdapterFailure> {
        let url = format!(
            "{}/{}/{}",
            self.config.api_base, self.config.graph_version, path
        );
        let response = self.authorized_get(&url)?;
        self.parse_graph_response(response)
    }

    fn authorized_get(
        &self,
        url: &str,
    ) -> Result<ureq::http::Response<ureq::Body>, AdapterFailure> {
        let response = self.access_token.with_bytes(|token| {
            let token = std::str::from_utf8(token)
                .map_err(|_| authentication("WhatsApp access token is not UTF-8"))?;
            self.http
                .get(url)
                .header("Authorization", format!("Bearer {token}"))
                .call()
                .map_err(|error| transport_failure(&error))
        })?;
        if response.status().is_success() {
            Ok(response)
        } else {
            Err(classify_graph_status(response.status().as_u16(), None))
        }
    }

    fn graph_post(&self, path: &str, payload: &Value) -> Result<Value, AdapterFailure> {
        let body = serde_json::to_vec(payload)
            .map_err(|_| malformed("WhatsApp request payload could not be encoded"))?;
        self.graph_post_bytes(path, &body, "application/json")
    }

    fn graph_post_bytes(
        &self,
        path: &str,
        body: &[u8],
        content_type: &str,
    ) -> Result<Value, AdapterFailure> {
        let url = format!(
            "{}/{}/{}",
            self.config.api_base, self.config.graph_version, path
        );
        let response = self.access_token.with_bytes(|token| {
            let token = std::str::from_utf8(token)
                .map_err(|_| authentication("WhatsApp access token is not UTF-8"))?;
            self.http
                .post(&url)
                .header("Authorization", format!("Bearer {token}"))
                .header("Content-Type", content_type)
                .send(body)
                .map_err(|error| transport_failure(&error))
        })?;
        self.parse_graph_response(response)
    }

    fn parse_graph_response(
        &self,
        mut response: ureq::http::Response<ureq::Body>,
    ) -> Result<Value, AdapterFailure> {
        let status = response.status().as_u16();
        let body = response
            .body_mut()
            .with_config()
            .limit(u64::try_from(self.config.max_event_bytes).unwrap_or(u64::MAX))
            .read_to_vec()
            .map_err(|error| transport_failure(&error))?;
        let parsed = serde_json::from_slice::<Value>(&body).ok();
        if !(200..=299).contains(&status) {
            return Err(classify_graph_status(status, parsed.as_ref()));
        }
        parsed.ok_or_else(|| malformed("WhatsApp returned malformed JSON"))
    }
}

impl ChannelAdapter for WhatsAppCloudAdapter {
    fn features(&self) -> AdapterFeatures {
        AdapterFeatures {
            capabilities: BTreeSet::from([
                AdapterCapability::Attachments,
                AdapterCapability::Steering,
                AdapterCapability::Cancellation,
                AdapterCapability::Reconnect,
            ]),
            max_attachment_bytes: self.config.max_attachment_bytes,
            requests_per_minute: None,
        }
    }

    fn receive(&mut self) -> Result<AdapterEvent, AdapterFailure> {
        self.pending
            .pop_front()
            .ok_or_else(|| retryable("no WhatsApp webhook event is queued"))
    }

    fn send(&mut self, message: &OutboundMessage) -> Result<SendReceipt, AdapterFailure> {
        self.send_message(message)
    }

    fn reconnect(&mut self) -> Result<(), AdapterFailure> {
        let result = self.graph_get(&self.config.phone_number_id)?;
        if result.get("id").and_then(Value::as_str) == Some(&self.config.phone_number_id) {
            Ok(())
        } else {
            Err(permission(
                "WhatsApp health response belongs to another phone number",
            ))
        }
    }
}

impl ChannelAdapterV2 for WhatsAppCloudAdapter {
    fn capabilities_v2(&self) -> ChannelCapabilitiesV2 {
        whatsapp_capabilities(&self.config)
    }

    fn receive_v2(&mut self) -> Result<ChannelEventV2, ChannelAdapterErrorV2> {
        self.pending_v2.pop_front().ok_or_else(|| {
            v2_error(
                ChannelAdapterErrorKindV2::TransientNetwork,
                "no WhatsApp webhook event is queued",
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
            ChannelOperationV2::SendMessage(message) => self.send_v2_message(message),
            ChannelOperationV2::Cancel { cancellation_id } => {
                if cancellation_id.trim().is_empty() {
                    return Err(ChannelAdapterErrorV2::malformed(
                        "WhatsApp cancellation identity is empty",
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
                "WhatsApp Cloud operation is not supported",
            )),
        }
    }

    fn reconnect_v2(&mut self) -> Result<(), ChannelAdapterErrorV2> {
        self.reconnect().map_err(adapter_failure_to_v2)
    }

    fn reconnect_cursor_v2(&self) -> Option<ReconnectCursorV2> {
        self.cursor
            .recent_message_ids
            .last()
            .zip(self.cursor.last_event_at)
            .map(|(message_id, observed_at)| ReconnectCursorV2 {
                value: message_id.clone(),
                observed_at,
            })
    }
}

impl WhatsAppCloudAdapter {
    fn send_v2_message(
        &mut self,
        message: &ChannelOutboundMessageV2,
    ) -> Result<ChannelOperationReceiptV2, ChannelAdapterErrorV2> {
        if message.idempotency_key.trim().is_empty() {
            return Err(ChannelAdapterErrorV2::malformed(
                "WhatsApp delivery identity is empty",
            ));
        }
        if self.cancelled.contains(&message.idempotency_key) {
            return Err(v2_error(
                ChannelAdapterErrorKindV2::Cancelled,
                "WhatsApp delivery was cancelled before dispatch",
            ));
        }
        if !message.rich_content.is_empty() {
            return Err(ChannelAdapterErrorV2::unsupported(
                "WhatsApp rich content translation is not implemented",
            ));
        }
        let receipt = self
            .send_message(&OutboundMessage {
                route: message.route.clone(),
                idempotency_key: message.idempotency_key.clone(),
                text: message.text.clone(),
                artifacts: message.artifacts.clone(),
            })
            .map_err(adapter_failure_to_v2)?;
        Ok(operation_receipt(
            message.idempotency_key.clone(),
            Some(receipt.platform_message_id),
            ChannelReceiptStateV2::Accepted,
            receipt.duplicate_possible,
        ))
    }
}

fn whatsapp_message_event_v2(raw_message: &Value, message: &InboundMessage) -> ChannelEventV2 {
    let conversation = ChannelConversationV2 {
        platform_id: message.conversation.clone(),
        kind: ChannelConversationKindV2::Direct,
        thread_id: None,
        reply_to_message_id: message.reply_target.clone(),
    };
    let sender = ChannelIdentityV2 {
        platform_id: message.sender.clone(),
        display_name: None,
        is_bot: false,
    };
    let attachments = message
        .attachments
        .iter()
        .map(|attachment| {
            let kind = if raw_message
                .pointer("/audio/voice")
                .and_then(Value::as_bool)
                .unwrap_or(false)
            {
                ChannelAttachmentKindV2::Voice
            } else if attachment.media_type.starts_with("image/") {
                ChannelAttachmentKindV2::Image
            } else if attachment.media_type.starts_with("audio/") {
                ChannelAttachmentKindV2::Audio
            } else if attachment.media_type.starts_with("video/") {
                ChannelAttachmentKindV2::Video
            } else {
                ChannelAttachmentKindV2::File
            };
            ChannelAttachmentV2 {
                attachment: attachment.clone(),
                kind,
                duration_ms: None,
                metadata: BTreeMap::new(),
            }
        })
        .collect::<Vec<_>>();
    let message_type = raw_message
        .get("type")
        .and_then(Value::as_str)
        .unwrap_or("unknown");
    let mut metadata = BTreeMap::new();
    metadata.insert("whatsapp_message_type".to_owned(), message_type.to_owned());
    let event = if let Some((name, arguments)) = parse_command(&message.text) {
        ChannelEventKindV2::Command(ChannelCommandV2 {
            command_id: format!("whatsapp:{}", message.message_id),
            account_id: message.external_account.clone(),
            conversation,
            sender,
            name,
            arguments,
            occurred_at: message.occurred_at,
            metadata,
        })
    } else {
        ChannelEventKindV2::MessageCreated(ChannelMessageV2 {
            message_id: message.message_id.clone(),
            account_id: message.external_account.clone(),
            conversation,
            sender,
            text: message.text.clone(),
            attachments,
            rich_content: Vec::new(),
            mentions: Vec::new(),
            occurred_at: message.occurred_at,
            metadata,
        })
    };
    ChannelEventV2 {
        contract: CHANNEL_CONTRACT_V2,
        event_id: format!("whatsapp:{}", message.message_id),
        delivery_attempt: 1,
        event,
        metadata: BTreeMap::new(),
    }
}

fn whatsapp_status_event_v2(status: &WhatsAppDeliveryStatus) -> ChannelEventV2 {
    let state = match status.state {
        WhatsAppDeliveryState::Sent => ChannelReceiptStateV2::Accepted,
        WhatsAppDeliveryState::Delivered => ChannelReceiptStateV2::Delivered,
        WhatsAppDeliveryState::Read => ChannelReceiptStateV2::Read,
        WhatsAppDeliveryState::Failed => ChannelReceiptStateV2::Failed,
        WhatsAppDeliveryState::Deleted => ChannelReceiptStateV2::Cancelled,
    };
    let mut metadata = BTreeMap::new();
    if !status.safe_error_codes.is_empty() {
        metadata.insert(
            "whatsapp_error_codes".to_owned(),
            status
                .safe_error_codes
                .iter()
                .map(u64::to_string)
                .collect::<Vec<_>>()
                .join(","),
        );
    }
    ChannelEventV2 {
        contract: CHANNEL_CONTRACT_V2,
        event_id: format!(
            "whatsapp-status:{}:{:?}:{}",
            status.message_id,
            status.state,
            status.occurred_at.unix_millis()
        ),
        delivery_attempt: 1,
        event: ChannelEventKindV2::Receipt {
            account_id: status.account_id.clone(),
            conversation: ChannelConversationV2 {
                platform_id: status.recipient.clone(),
                kind: ChannelConversationKindV2::Direct,
                thread_id: None,
                reply_to_message_id: None,
            },
            platform_message_id: status.message_id.clone(),
            state,
        },
        metadata,
    }
}

fn whatsapp_attachment(
    media: &Value,
    kind: &str,
    max_attachment_bytes: u64,
) -> Result<Attachment, AdapterFailure> {
    let byte_length = media.get("file_size").and_then(Value::as_u64).unwrap_or(0);
    if byte_length > max_attachment_bytes {
        return Err(permanent("WhatsApp inbound media is oversized"));
    }
    let default_media_type = match kind {
        "audio" => "audio/ogg",
        "image" => "image/jpeg",
        "sticker" => "image/webp",
        "video" => "video/mp4",
        _ => "application/octet-stream",
    };
    Ok(Attachment {
        id: string_field(media, "id")?,
        file_name: media
            .get("filename")
            .and_then(Value::as_str)
            .unwrap_or(match kind {
                "audio" => "whatsapp-audio.ogg",
                "image" => "whatsapp-image.jpg",
                "sticker" => "whatsapp-sticker.webp",
                "video" => "whatsapp-video.mp4",
                _ => "whatsapp-document",
            })
            .to_owned(),
        media_type: media
            .get("mime_type")
            .and_then(Value::as_str)
            .unwrap_or(default_media_type)
            .to_owned(),
        byte_length,
        artifact_id: None,
        download_url: None,
        staging_file: None,
        sha256: media
            .get("sha256")
            .and_then(Value::as_str)
            .map(str::to_owned),
    })
}

fn normalize_status(
    status: &Value,
    account_id: &str,
) -> Result<WhatsAppDeliveryStatus, AdapterFailure> {
    let state = match string_field(status, "status")?.as_str() {
        "sent" => WhatsAppDeliveryState::Sent,
        "delivered" => WhatsAppDeliveryState::Delivered,
        "read" => WhatsAppDeliveryState::Read,
        "failed" => WhatsAppDeliveryState::Failed,
        "deleted" => WhatsAppDeliveryState::Deleted,
        _ => return Err(unsupported("WhatsApp delivery status is unsupported")),
    };
    let occurred_at = status
        .get("timestamp")
        .and_then(Value::as_str)
        .and_then(|timestamp| timestamp.parse::<i64>().ok())
        .and_then(|seconds| seconds.checked_mul(1_000))
        .map_or_else(now, UtcTimestamp::from_unix_millis);
    let safe_error_codes = status
        .get("errors")
        .and_then(Value::as_array)
        .map(|errors| {
            errors
                .iter()
                .filter_map(|error| error.get("code").and_then(Value::as_u64))
                .collect()
        })
        .unwrap_or_default();
    Ok(WhatsAppDeliveryStatus {
        account_id: account_id.to_owned(),
        message_id: string_field(status, "id")?,
        recipient: string_field(status, "recipient_id")?,
        state,
        occurred_at,
        safe_error_codes,
    })
}

fn send_receipt(result: &Value) -> Result<SendReceipt, AdapterFailure> {
    let platform_message_id = result
        .get("messages")
        .and_then(Value::as_array)
        .and_then(|messages| messages.first())
        .and_then(|message| message.get("id"))
        .and_then(Value::as_str)
        .filter(|id| !id.is_empty())
        .ok_or_else(|| malformed("WhatsApp send receipt has no message identity"))?
        .to_owned();
    Ok(SendReceipt {
        platform_message_id,
        accepted_at: now(),
        duplicate_possible: true,
    })
}

fn multipart_text(
    body: &mut Vec<u8>,
    boundary: &str,
    name: &str,
    value: &str,
) -> Result<(), AdapterFailure> {
    write!(
        body,
        "--{boundary}\r\nContent-Disposition: form-data; name=\"{name}\"\r\n\r\n{value}\r\n"
    )
    .map_err(|_| malformed("WhatsApp multipart payload could not be encoded"))
}

fn decode_signature(header: &str) -> Option<Vec<u8>> {
    let hex = header.strip_prefix("sha256=")?;
    if hex.len() != 64 {
        return None;
    }
    hex.as_bytes()
        .chunks_exact(2)
        .map(|pair| {
            let high = hex_nibble(pair[0])?;
            let low = hex_nibble(pair[1])?;
            Some((high << 4) | low)
        })
        .collect()
}

fn secret_matches(expected: &[u8], presented: &[u8]) -> bool {
    constant_time_eq(expected, presented)
}

#[cfg(test)]
pub(crate) fn webhook_signature(secret: &[u8], body: &[u8]) -> String {
    format!("sha256={}", hex_encode(&hmac_sha256(secret, body)))
}

fn hmac_sha256(key: &[u8], message: &[u8]) -> [u8; 32] {
    let mut authenticator =
        Hmac::<Sha256>::new_from_slice(key).expect("HMAC accepts keys of every length");
    authenticator.update(message);
    authenticator.finalize().into_bytes().into()
}

#[cfg(test)]
fn hex_encode(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut encoded = String::with_capacity(bytes.len() * 2);
    for &byte in bytes {
        encoded.push(char::from(HEX[usize::from(byte >> 4)]));
        encoded.push(char::from(HEX[usize::from(byte & 0x0f)]));
    }
    encoded
}

fn constant_time_eq(left: &[u8], right: &[u8]) -> bool {
    let mut difference = left.len() ^ right.len();
    let max_len = left.len().max(right.len());
    for index in 0..max_len {
        difference |= usize::from(
            left.get(index).copied().unwrap_or_default()
                ^ right.get(index).copied().unwrap_or_default(),
        );
    }
    difference == 0
}

const fn hex_nibble(byte: u8) -> Option<u8> {
    match byte {
        b'0'..=b'9' => Some(byte - b'0'),
        b'a'..=b'f' => Some(byte - b'a' + 10),
        b'A'..=b'F' => Some(byte - b'A' + 10),
        _ => None,
    }
}

fn safe_media_url(url: &str) -> bool {
    url.starts_with("https://")
        || url.starts_with("http://127.0.0.1:")
        || url.starts_with("http://localhost:")
}

fn safe_graph_identifier(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 256
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b'-'))
}

fn valid_graph_version(value: &str) -> bool {
    let Some((major, minor)) = value
        .strip_prefix('v')
        .and_then(|value| value.split_once('.'))
    else {
        return false;
    };
    !major.is_empty()
        && !minor.is_empty()
        && major.bytes().all(|byte| byte.is_ascii_digit())
        && minor.bytes().all(|byte| byte.is_ascii_digit())
}

fn bounded_order(values: &[String], capacity: usize) -> VecDeque<String> {
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

fn remember(
    value: String,
    seen: &mut BTreeSet<String>,
    order: &mut VecDeque<String>,
    capacity: usize,
) {
    seen.insert(value.clone());
    order.push_back(value);
    while order.len() > capacity {
        if let Some(expired) = order.pop_front() {
            seen.remove(&expired);
        }
    }
}

fn command_intent(text: &str) -> InboundIntent {
    match text.split_ascii_whitespace().next().unwrap_or_default() {
        "/cancel" | "/stop" => InboundIntent::Cancel,
        "/steer" => InboundIntent::Steer,
        _ => InboundIntent::Prompt,
    }
}

fn parse_command(text: &str) -> Option<(String, String)> {
    let mut parts = text.splitn(2, char::is_whitespace);
    let name = parts.next()?.strip_prefix('/')?.trim();
    if name.is_empty() {
        return None;
    }
    Some((
        name.to_owned(),
        parts.next().unwrap_or_default().trim().to_owned(),
    ))
}

fn string_field(value: &Value, field: &str) -> Result<String, AdapterFailure> {
    value
        .get(field)
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty())
        .map(str::to_owned)
        .ok_or_else(|| malformed("WhatsApp payload is missing an identity field"))
}

fn whatsapp_capabilities(config: &WhatsAppCloudConfig) -> ChannelCapabilitiesV2 {
    let unsupported = |safe_reason: &str| ChannelCapabilitySupportV2::Unsupported {
        safe_reason: safe_reason.to_owned(),
    };
    let mut declarations = ChannelCapabilityV2::ALL
        .into_iter()
        .map(|capability| {
            (
                capability,
                unsupported("WhatsApp Cloud adapter does not implement this capability"),
            )
        })
        .collect::<BTreeMap<_, _>>();
    for capability in [
        ChannelCapabilityV2::InboundMessages,
        ChannelCapabilityV2::OutboundMessages,
        ChannelCapabilityV2::Replies,
        ChannelCapabilityV2::Commands,
        ChannelCapabilityV2::Attachments,
        ChannelCapabilityV2::Voice,
        ChannelCapabilityV2::DeliveryReceipts,
        ChannelCapabilityV2::ReadReceipts,
        ChannelCapabilityV2::RateLimits,
        ChannelCapabilityV2::Reconnect,
        ChannelCapabilityV2::Cancellation,
    ] {
        declarations.insert(capability, ChannelCapabilitySupportV2::Supported);
    }
    declarations.insert(
        ChannelCapabilityV2::Threads,
        unsupported("WhatsApp Cloud conversations do not expose message threads"),
    );
    declarations.insert(
        ChannelCapabilityV2::Mentions,
        unsupported("WhatsApp Cloud does not expose general mention entities"),
    );
    declarations.insert(
        ChannelCapabilityV2::MessageEdits,
        unsupported("WhatsApp Cloud adapter does not edit sent messages"),
    );
    declarations.insert(
        ChannelCapabilityV2::MessageDeletion,
        unsupported("WhatsApp Cloud adapter does not delete sent messages"),
    );
    declarations.insert(
        ChannelCapabilityV2::Reactions,
        unsupported("WhatsApp reaction messages are not enabled by this adapter"),
    );
    declarations.insert(
        ChannelCapabilityV2::RichContent,
        unsupported("WhatsApp templates use the explicit template operation"),
    );
    declarations.insert(
        ChannelCapabilityV2::Typing,
        unsupported("WhatsApp Cloud does not expose a bot typing operation"),
    );
    declarations.insert(
        ChannelCapabilityV2::IdempotentSend,
        unsupported("WhatsApp Cloud does not accept an idempotency key"),
    );
    ChannelCapabilitiesV2 {
        contract: ChannelContractVersion::new(2, 0),
        declarations,
        max_event_bytes: u64::try_from(config.max_event_bytes).unwrap_or(u64::MAX),
        max_attachment_bytes: config.max_attachment_bytes,
        max_attachments: 1,
        max_rich_content_bytes: 1,
        requests_per_minute: None,
    }
}

fn operation_receipt(
    operation_id: String,
    platform_message_id: Option<String>,
    state: ChannelReceiptStateV2,
    duplicate_possible: bool,
) -> ChannelOperationReceiptV2 {
    ChannelOperationReceiptV2 {
        operation_id,
        platform_message_id,
        accepted_at: now(),
        state,
        duplicate_possible,
        metadata: BTreeMap::new(),
    }
}

fn event_occurred_at(event: &ChannelEventV2) -> Option<UtcTimestamp> {
    match &event.event {
        ChannelEventKindV2::MessageCreated(message) => Some(message.occurred_at),
        ChannelEventKindV2::Command(command) => Some(command.occurred_at),
        _ => None,
    }
}

fn adapter_failure_to_v2(failure: AdapterFailure) -> ChannelAdapterErrorV2 {
    let message = failure.safe_message.to_ascii_lowercase();
    let kind = match failure.class {
        RetryClass::RateLimited => ChannelAdapterErrorKindV2::RateLimit,
        RetryClass::Retryable | RetryClass::Reconnect => {
            ChannelAdapterErrorKindV2::TransientNetwork
        }
        RetryClass::Permanent
            if message.contains("authentication") || message.contains("token") =>
        {
            ChannelAdapterErrorKindV2::Authentication
        }
        RetryClass::Permanent
            if message.contains("permission")
                || message.contains("does not belong")
                || message.contains("another phone")
                || message.contains("another business") =>
        {
            ChannelAdapterErrorKindV2::Permission
        }
        RetryClass::Permanent if message.contains("unsupported") => {
            ChannelAdapterErrorKindV2::UnsupportedFeature
        }
        RetryClass::Permanent
            if message.contains("malformed")
                || message.contains("missing")
                || message.contains("unsafe")
                || message.contains("invalid") =>
        {
            ChannelAdapterErrorKindV2::MalformedEvent
        }
        RetryClass::Permanent => ChannelAdapterErrorKindV2::PermanentDestination,
    };
    ChannelAdapterErrorV2 {
        kind,
        safe_message: failure.safe_message,
        retry_after_ms: failure.retry_after_ms,
    }
}

fn v2_to_adapter_failure(error: ChannelAdapterErrorV2) -> AdapterFailure {
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

fn v2_error(kind: ChannelAdapterErrorKindV2, message: &str) -> ChannelAdapterErrorV2 {
    ChannelAdapterErrorV2 {
        kind,
        safe_message: message.to_owned(),
        retry_after_ms: None,
    }
}

fn classify_graph_status(status: u16, body: Option<&Value>) -> AdapterFailure {
    let code = body
        .and_then(|value| value.pointer("/error/code"))
        .and_then(Value::as_u64)
        .unwrap_or(u64::from(status));
    let transient = body
        .and_then(|value| value.pointer("/error/is_transient"))
        .and_then(Value::as_bool)
        .unwrap_or(false);
    if matches!(status, 401) || code == 190 {
        authentication("WhatsApp authentication failed")
    } else if matches!(status, 403) || matches!(code, 10 | 200) {
        permission("WhatsApp permission was denied")
    } else if status == 429 || matches!(code, 4 | 80007 | 130_429 | 131_048) {
        AdapterFailure {
            class: RetryClass::RateLimited,
            safe_message: "WhatsApp rate limit reached".to_owned(),
            retry_after_ms: None,
        }
    } else if transient || (500..=599).contains(&status) {
        retryable("WhatsApp service is temporarily unavailable")
    } else if matches!(code, 131_026 | 131_047 | 131_051) {
        permanent("WhatsApp destination or message is unavailable")
    } else {
        permanent("WhatsApp rejected the request")
    }
}

fn transport_failure(error: &ureq::Error) -> AdapterFailure {
    match error {
        ureq::Error::Timeout(_) => retryable("WhatsApp request timed out"),
        _ => reconnect_failure("WhatsApp transport failed"),
    }
}

fn malformed(message: &str) -> AdapterFailure {
    permanent(message)
}

fn authentication(message: &str) -> AdapterFailure {
    permanent(message)
}

fn permission(message: &str) -> AdapterFailure {
    permanent(message)
}

fn unsupported(message: &str) -> AdapterFailure {
    permanent(message)
}

fn permanent(message: &str) -> AdapterFailure {
    AdapterFailure {
        class: RetryClass::Permanent,
        safe_message: message.to_owned(),
        retry_after_ms: None,
    }
}

fn retryable(message: &str) -> AdapterFailure {
    AdapterFailure {
        class: RetryClass::Retryable,
        safe_message: message.to_owned(),
        retry_after_ms: None,
    }
}

fn reconnect_failure(message: &str) -> AdapterFailure {
    AdapterFailure {
        class: RetryClass::Reconnect,
        safe_message: message.to_owned(),
        retry_after_ms: None,
    }
}

fn now() -> UtcTimestamp {
    UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn whatsapp_graph_error_taxonomy_preserves_auth_rate_limit_and_destination() {
        let revoked = adapter_failure_to_v2(classify_graph_status(
            400,
            Some(&json!({"error": {"code": 190}})),
        ));
        assert_eq!(revoked.kind, ChannelAdapterErrorKindV2::Authentication);
        let rate_limit = adapter_failure_to_v2(classify_graph_status(
            400,
            Some(&json!({"error": {"code": 130_429}})),
        ));
        assert_eq!(rate_limit.kind, ChannelAdapterErrorKindV2::RateLimit);
        let destination = adapter_failure_to_v2(classify_graph_status(
            400,
            Some(&json!({"error": {"code": 131_026}})),
        ));
        assert_eq!(
            destination.kind,
            ChannelAdapterErrorKindV2::PermanentDestination
        );
    }

    #[test]
    fn whatsapp_signature_decoder_and_media_request_are_bounded() {
        let signature = format!("sha256={}", "00".repeat(32));
        assert_eq!(decode_signature(&signature), Some(vec![0; 32]));
        assert!(decode_signature("sha256=bad").is_none());
        let mut body = Vec::new();
        multipart_text(&mut body, "boundary", "messaging_product", "whatsapp")
            .expect("multipart text");
        let body = String::from_utf8(body).expect("test multipart is UTF-8");
        assert!(body.contains("messaging_product"));
        assert!(body.contains("whatsapp"));
        assert!(!body.contains("access-token"));
    }

    #[test]
    fn whatsapp_hmac_sha256_matches_rfc_4231() {
        let key = [0x0b; 20];
        assert_eq!(
            webhook_signature(&key, b"Hi There"),
            "sha256=b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7"
        );
    }
}
