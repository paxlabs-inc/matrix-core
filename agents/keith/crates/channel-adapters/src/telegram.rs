use std::collections::{BTreeMap, BTreeSet, VecDeque};
use std::io::Write as _;
use std::time::Duration;

use keith_agent_types::{ArtifactId, UtcTimestamp};
use keith_channel_core::{
    AdapterCapability, AdapterEvent, AdapterFailure, AdapterFeatures, Attachment,
    CHANNEL_CONTRACT_V2, ChannelAccountSetupV2, ChannelAdapter, ChannelAdapterErrorKindV2,
    ChannelAdapterErrorV2, ChannelAdapterV2, ChannelAttachmentKindV2, ChannelAttachmentV2,
    ChannelCapabilitiesV2, ChannelCapabilitySupportV2, ChannelCapabilityV2, ChannelCommandV2,
    ChannelConnectionHealthV2, ChannelContractVersion, ChannelConversationKindV2,
    ChannelConversationV2, ChannelEventKindV2, ChannelEventV2, ChannelIdentityV2,
    ChannelMessageEditV2, ChannelMessageV2, ChannelOperationReceiptV2, ChannelOperationV2,
    ChannelOutboundMessageV2, ChannelReceiptStateV2, InboundIntent, InboundMessage,
    OutboundMessage, ReconnectCursorV2, ReplyRoute, RetryClass, SendReceipt,
};
use keith_credentials::SecretValue;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

const TELEGRAM_CHANNEL: &str = "telegram";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TelegramIngress {
    Webhook,
    Polling { timeout_seconds: u16, limit: u8 },
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TelegramConfig {
    pub api_base: String,
    pub bot_account_id: String,
    pub ingress: TelegramIngress,
    pub max_event_bytes: usize,
    pub max_attachment_bytes: u64,
    pub max_staged_bytes: usize,
    pub timeout_ms: u64,
    pub deduplication_capacity: usize,
    pub max_pending_events: usize,
}

impl TelegramConfig {
    pub fn production(bot_account_id: impl Into<String>, ingress: TelegramIngress) -> Self {
        Self {
            api_base: "https://api.telegram.org".to_owned(),
            bot_account_id: bot_account_id.into(),
            ingress,
            max_event_bytes: 1024 * 1_024,
            max_attachment_bytes: 20 * 1_024 * 1_024,
            max_staged_bytes: 50 * 1_024 * 1_024,
            timeout_ms: 35_000,
            deduplication_capacity: 4_096,
            max_pending_events: 1_024,
        }
    }
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TelegramCursor {
    pub next_update_id: Option<i64>,
    pub recent_update_ids: Vec<i64>,
    #[serde(default)]
    pub last_update_at: Option<UtcTimestamp>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TelegramUploadKind {
    Document,
    Audio,
    Voice,
    Photo,
    Video,
}

impl TelegramUploadKind {
    const fn method_and_field(self) -> (&'static str, &'static str) {
        match self {
            Self::Document => ("sendDocument", "document"),
            Self::Audio => ("sendAudio", "audio"),
            Self::Voice => ("sendVoice", "voice"),
            Self::Photo => ("sendPhoto", "photo"),
            Self::Video => ("sendVideo", "video"),
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TelegramUpload {
    pub kind: TelegramUploadKind,
    pub file_name: String,
    pub media_type: String,
    pub bytes: Vec<u8>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TelegramDownload {
    pub file_name: String,
    pub bytes: Vec<u8>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TelegramWebhookOutcome {
    Enqueued,
    Duplicate,
    Ignored,
}

pub struct TelegramAdapter {
    config: TelegramConfig,
    token: SecretValue,
    webhook_secret: SecretValue,
    http: ureq::Agent,
    cursor: TelegramCursor,
    seen: BTreeSet<i64>,
    seen_order: VecDeque<i64>,
    pending: VecDeque<AdapterEvent>,
    pending_v2: VecDeque<ChannelEventV2>,
    staged: BTreeMap<ArtifactId, TelegramUpload>,
    staged_bytes: usize,
    cancelled: BTreeSet<String>,
}

impl TelegramAdapter {
    /// Creates a profile-owned Telegram adapter without exposing either secret.
    ///
    /// # Errors
    ///
    /// Returns a permanent failure for unsafe endpoints, invalid bounds, or an invalid webhook
    /// secret.
    pub fn new(
        mut config: TelegramConfig,
        token: SecretValue,
        webhook_secret: SecretValue,
        cursor: TelegramCursor,
    ) -> Result<Self, AdapterFailure> {
        let api_base_len = config.api_base.trim_end_matches('/').len();
        config.api_base.truncate(api_base_len);
        let ingress_valid = match config.ingress {
            TelegramIngress::Webhook => true,
            TelegramIngress::Polling {
                timeout_seconds,
                limit,
            } => timeout_seconds > 0 && limit > 0 && limit <= 100,
        };
        let endpoint_valid = config.api_base.starts_with("https://")
            || config.api_base.starts_with("http://127.0.0.1:")
            || config.api_base.starts_with("http://localhost:");
        let token_valid = token.with_bytes(safe_bot_token);
        let secret_valid = webhook_secret.with_bytes(|secret| {
            (1..=256).contains(&secret.len())
                && secret
                    .iter()
                    .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
        });
        if config.bot_account_id.trim().is_empty()
            || !ingress_valid
            || !endpoint_valid
            || !token_valid
            || !secret_valid
            || config.max_event_bytes == 0
            || config.max_attachment_bytes == 0
            || config.max_staged_bytes == 0
            || config.timeout_ms == 0
            || config.deduplication_capacity == 0
            || config.max_pending_events == 0
        {
            return Err(permanent("invalid Telegram adapter configuration"));
        }
        let seen_order = cursor
            .recent_update_ids
            .iter()
            .rev()
            .take(config.deduplication_capacity)
            .copied()
            .collect::<Vec<_>>()
            .into_iter()
            .rev()
            .collect::<VecDeque<_>>();
        let seen = seen_order.iter().copied().collect();
        let http: ureq::Agent = ureq::Agent::config_builder()
            .timeout_global(Some(Duration::from_millis(config.timeout_ms)))
            .http_status_as_error(false)
            .build()
            .into();
        let adapter = Self {
            config,
            token,
            webhook_secret,
            http,
            cursor,
            seen,
            seen_order,
            pending: VecDeque::new(),
            pending_v2: VecDeque::new(),
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

    pub const fn cursor(&self) -> &TelegramCursor {
        &self.cursor
    }

    pub fn account_setup_v2(&self) -> ChannelAccountSetupV2 {
        ChannelAccountSetupV2 {
            account_id: self.config.bot_account_id.clone(),
            required_credential_names: BTreeSet::from([
                "bot_token".to_owned(),
                "webhook_secret".to_owned(),
            ]),
            required_scopes: BTreeSet::new(),
            webhook_configured: matches!(self.config.ingress, TelegramIngress::Webhook),
            socket_or_polling_configured: matches!(
                self.config.ingress,
                TelegramIngress::Polling { .. }
            ),
            connection_health: ChannelConnectionHealthV2::Disconnected,
            reconnect_cursor_present: self.cursor.next_update_id.is_some()
                || !self.cursor.recent_update_ids.is_empty(),
            safe_test_supported: true,
            metadata: BTreeMap::from([(
                "ingress".to_owned(),
                match self.config.ingress {
                    TelegramIngress::Webhook => "webhook",
                    TelegramIngress::Polling { .. } => "polling",
                }
                .to_owned(),
            )]),
        }
    }

    /// Runs Telegram's read-only `getMe` operation and verifies bot-account continuity.
    ///
    /// # Errors
    ///
    /// Returns a classified error for transport, authentication, malformed data, or mismatch.
    pub fn test_connection(&self) -> Result<(), ChannelAdapterErrorV2> {
        let result = self
            .api_call("getMe", &json!({}))
            .map_err(adapter_failure_to_v2)?;
        let account = result
            .get("id")
            .and_then(Value::as_i64)
            .map(|value| value.to_string())
            .or_else(|| result.get("id").and_then(Value::as_str).map(str::to_owned))
            .ok_or_else(|| ChannelAdapterErrorV2::malformed("Telegram getMe omitted bot ID"))?;
        if account == self.config.bot_account_id {
            Ok(())
        } else {
            Err(v2_error(
                ChannelAdapterErrorKindV2::Permission,
                "Telegram credential belongs to another bot account",
            ))
        }
    }

    /// Replaces the Bot API credential in memory. The previous secret is zeroed on drop.
    pub fn rotate_token(&mut self, token: SecretValue) {
        self.token = token;
    }

    /// Verifies the Telegram secret header before parsing and queues one normalized update.
    ///
    /// # Errors
    ///
    /// Returns a permanent failure for a mismatched secret, oversized body, or malformed update.
    pub fn ingest_webhook(
        &mut self,
        secret_header: &[u8],
        body: &[u8],
    ) -> Result<TelegramWebhookOutcome, AdapterFailure> {
        if body.len() > self.config.max_event_bytes {
            return Err(permanent("Telegram webhook body exceeded its bound"));
        }
        let valid = self
            .webhook_secret
            .with_bytes(|expected| secret_matches(expected, secret_header));
        if !valid {
            return Err(authentication("Telegram webhook authentication failed"));
        }
        let update = serde_json::from_slice::<Value>(body)
            .map_err(|_| malformed("Telegram webhook body is malformed"))?;
        self.accept_update(&update)
    }

    /// Stages bounded bytes for a single outbound Telegram media send.
    ///
    /// # Errors
    ///
    /// Returns a permanent failure for unsafe metadata or an exceeded byte budget.
    pub fn stage_artifact(
        &mut self,
        id: ArtifactId,
        upload: TelegramUpload,
    ) -> Result<(), AdapterFailure> {
        if upload.file_name.trim().is_empty()
            || upload.media_type.trim().is_empty()
            || upload.file_name.contains(['\r', '\n', '"'])
            || upload.media_type.contains(['\r', '\n'])
            || upload.bytes.is_empty()
            || u64::try_from(upload.bytes.len()).unwrap_or(u64::MAX)
                > self.config.max_attachment_bytes
        {
            return Err(permanent("Telegram attachment is invalid or oversized"));
        }
        let old = self.staged.get(&id).map_or(0, |value| value.bytes.len());
        let next = self
            .staged_bytes
            .saturating_sub(old)
            .saturating_add(upload.bytes.len());
        if next > self.config.max_staged_bytes {
            return Err(permanent("Telegram staged attachment budget exceeded"));
        }
        self.staged.insert(id, upload);
        self.staged_bytes = next;
        Ok(())
    }

    /// Downloads Telegram media through `getFile` without placing the bot token in normalized
    /// events or logs.
    ///
    /// # Errors
    ///
    /// Returns a classified API, transport, path, or media-bound failure.
    pub fn download_file(&self, file_id: &str) -> Result<TelegramDownload, AdapterFailure> {
        if file_id.trim().is_empty() {
            return Err(malformed("Telegram file identity is empty"));
        }
        let result = self.api_call("getFile", &json!({"file_id": file_id}))?;
        let file_path = result
            .get("file_path")
            .and_then(Value::as_str)
            .filter(|path| safe_file_path(path))
            .ok_or_else(|| malformed("Telegram returned an unsafe file path"))?;
        let mut response = self.token.with_bytes(|token| {
            let token = std::str::from_utf8(token)
                .map_err(|_| authentication("Telegram token is not UTF-8"))?;
            if !safe_bot_token(token.as_bytes()) {
                return Err(authentication("Telegram token is not URL-path safe"));
            }
            let url = format!("{}/file/bot{token}/{file_path}", self.config.api_base);
            self.http
                .get(&url)
                .call()
                .map_err(|error| transport_failure(&error))
        })?;
        if !response.status().is_success() {
            return Err(classify_status(response.status().as_u16(), None));
        }
        let bytes = response
            .body_mut()
            .with_config()
            .limit(self.config.max_attachment_bytes.saturating_add(1))
            .read_to_vec()
            .map_err(|error| transport_failure(&error))?;
        if bytes.is_empty()
            || u64::try_from(bytes.len()).unwrap_or(u64::MAX) > self.config.max_attachment_bytes
        {
            return Err(permanent("Telegram downloaded media exceeded its bound"));
        }
        let file_name = file_path
            .rsplit('/')
            .next()
            .filter(|name| !name.is_empty())
            .unwrap_or("telegram-file")
            .to_owned();
        Ok(TelegramDownload { file_name, bytes })
    }

    /// Sends Telegram's transient typing indicator for a route owned by this adapter.
    ///
    /// # Errors
    ///
    /// Returns a classified route or Bot API failure.
    pub fn send_typing(&self, route: &ReplyRoute) -> Result<(), AdapterFailure> {
        self.validate_route(route)?;
        let mut payload = json!({"chat_id": route.conversation, "action": "typing"});
        add_thread(&mut payload, route)?;
        self.api_call("sendChatAction", &payload)?;
        Ok(())
    }

    /// Edits a Telegram text message on an adapter-owned route.
    ///
    /// # Errors
    ///
    /// Returns a classified route, length, or Bot API failure.
    pub fn edit_message(
        &self,
        route: &ReplyRoute,
        message_id: &str,
        text: &str,
    ) -> Result<SendReceipt, AdapterFailure> {
        self.validate_route(route)?;
        if text.is_empty() || text.chars().count() > 4_096 {
            return Err(permanent("Telegram edit text exceeds its bound"));
        }
        let message_id = message_id
            .parse::<i64>()
            .map_err(|_| malformed("Telegram edit message identity is malformed"))?;
        let result = self.api_call(
            "editMessageText",
            &json!({"chat_id": route.conversation, "message_id": message_id, "text": text}),
        )?;
        receipt_from_message(&result, false)
    }

    fn validate_route(&self, route: &ReplyRoute) -> Result<(), AdapterFailure> {
        if route.channel != TELEGRAM_CHANNEL
            || route.external_account != self.config.bot_account_id
            || route.conversation.trim().is_empty()
        {
            return Err(permission(
                "Telegram route does not belong to this adapter account",
            ));
        }
        Ok(())
    }

    fn poll_once(&mut self) -> Result<(), AdapterFailure> {
        let TelegramIngress::Polling {
            timeout_seconds,
            limit,
        } = self.config.ingress
        else {
            return Err(retryable("no Telegram webhook event is queued"));
        };
        let mut payload = json!({
            "timeout": timeout_seconds,
            "limit": limit,
            "allowed_updates": ["message", "edited_message", "channel_post", "edited_channel_post"]
        });
        if let Some(offset) = self.cursor.next_update_id {
            payload["offset"] = json!(offset);
        }
        let result = self.api_call("getUpdates", &payload)?;
        let updates = result
            .as_array()
            .ok_or_else(|| malformed("Telegram polling result is malformed"))?;
        for update in updates {
            self.accept_update(update)?;
        }
        Ok(())
    }

    fn accept_update(&mut self, update: &Value) -> Result<TelegramWebhookOutcome, AdapterFailure> {
        let update_id = update
            .get("update_id")
            .and_then(Value::as_i64)
            .filter(|value| *value >= 0)
            .ok_or_else(|| malformed("Telegram update identity is malformed"))?;
        if self.seen.contains(&update_id) {
            return Ok(TelegramWebhookOutcome::Duplicate);
        }
        let (normalized, normalized_v2) = self.normalize_update(update)?;
        if normalized.is_some()
            && (self.pending.len() >= self.config.max_pending_events
                || self.pending_v2.len() >= self.config.max_pending_events)
        {
            return Err(retryable("Telegram inbound queue reached its bound"));
        }
        self.remember_update(update_id);
        self.cursor.next_update_id = Some(
            self.cursor
                .next_update_id
                .unwrap_or(0)
                .max(update_id.saturating_add(1)),
        );
        self.cursor.last_update_at = normalized_v2
            .as_ref()
            .and_then(event_occurred_at)
            .or_else(|| Some(now()));
        if let Some(event) = normalized_v2 {
            event
                .validate(&self.capabilities_v2())
                .map_err(v2_to_adapter_failure)?;
            self.pending_v2.push_back(event);
        }
        if let Some(event) = normalized {
            self.pending.push_back(event);
            Ok(TelegramWebhookOutcome::Enqueued)
        } else {
            Ok(TelegramWebhookOutcome::Ignored)
        }
    }

    fn normalize_update(
        &self,
        update: &Value,
    ) -> Result<(Option<AdapterEvent>, Option<ChannelEventV2>), AdapterFailure> {
        let message = [
            "message",
            "edited_message",
            "channel_post",
            "edited_channel_post",
        ]
        .into_iter()
        .find_map(|field| update.get(field).map(|message| (field, message)));
        let Some((update_kind, message)) = message else {
            return Ok((None, None));
        };
        let chat = message
            .get("chat")
            .ok_or_else(|| malformed("Telegram message has no chat"))?;
        let conversation = json_identity(chat.get("id"))?;
        let sender = message
            .get("from")
            .or_else(|| message.get("sender_chat"))
            .and_then(|value| value.get("id"))
            .map(|value| json_identity(Some(value)))
            .transpose()?
            .ok_or_else(|| malformed("Telegram message has no sender"))?;
        let message_id = json_identity(message.get("message_id"))?;
        let attachments = telegram_attachments(message, self.config.max_attachment_bytes)?;
        let text = message
            .get("text")
            .or_else(|| message.get("caption"))
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned();
        if text.trim().is_empty() && attachments.is_empty() {
            return Ok((None, None));
        }
        let occurred_at = message
            .get("date")
            .and_then(Value::as_i64)
            .and_then(|seconds| seconds.checked_mul(1_000))
            .map_or_else(now, UtcTimestamp::from_unix_millis);
        let intent = command_intent(&text);
        let inbound = InboundMessage {
            channel: TELEGRAM_CHANNEL.to_owned(),
            external_account: self.config.bot_account_id.clone(),
            conversation,
            thread: message
                .get("message_thread_id")
                .map(|value| json_identity(Some(value)))
                .transpose()?,
            sender,
            message_id,
            reply_target: message
                .get("reply_to_message")
                .and_then(|reply| reply.get("message_id"))
                .map(|value| json_identity(Some(value)))
                .transpose()?,
            text,
            attachments,
            occurred_at,
            intent,
        };
        let update_id = update
            .get("update_id")
            .and_then(Value::as_i64)
            .ok_or_else(|| malformed("Telegram update identity is malformed"))?;
        let event_v2 = telegram_event_v2(update_id, update_kind, message, &inbound);
        Ok((
            Some(AdapterEvent::Inbound(Box::new(inbound))),
            Some(event_v2),
        ))
    }

    fn remember_update(&mut self, update_id: i64) {
        self.seen.insert(update_id);
        self.seen_order.push_back(update_id);
        while self.seen_order.len() > self.config.deduplication_capacity {
            if let Some(expired) = self.seen_order.pop_front() {
                self.seen.remove(&expired);
            }
        }
        self.cursor.recent_update_ids = self.seen_order.iter().copied().collect();
    }

    fn send_message(&mut self, message: &OutboundMessage) -> Result<SendReceipt, AdapterFailure> {
        self.validate_route(&message.route)?;
        if message.artifacts.len() > 1 {
            return Err(unsupported(
                "Telegram adapter supports one staged media item per outbox entry",
            ));
        }
        let result = if let Some(artifact_id) = message.artifacts.first() {
            let upload = self
                .staged
                .get(artifact_id)
                .ok_or_else(|| malformed("Telegram artifact bytes were not staged"))?;
            if message.text.chars().count() > 1_024 {
                return Err(permanent("Telegram media caption exceeds 1024 characters"));
            }
            let (method, field) = upload.kind.method_and_field();
            let (body, content_type) = build_media_multipart(message, upload, field)?;
            let result = self.api_bytes(method, &body, &content_type)?;
            if let Some(removed) = self.staged.remove(artifact_id) {
                self.staged_bytes = self.staged_bytes.saturating_sub(removed.bytes.len());
            }
            result
        } else {
            if message.text.is_empty() || message.text.chars().count() > 4_096 {
                return Err(permanent("Telegram message text exceeds its bound"));
            }
            let mut payload = json!({
                "chat_id": message.route.conversation,
                "text": message.text,
            });
            add_thread(&mut payload, &message.route)?;
            add_reply(&mut payload, &message.route)?;
            self.api_call("sendMessage", &payload)?
        };
        receipt_from_message(&result, true)
    }

    fn api_call(&self, method: &str, payload: &Value) -> Result<Value, AdapterFailure> {
        let body = serde_json::to_vec(payload)
            .map_err(|_| malformed("Telegram request payload could not be encoded"))?;
        self.api_bytes(method, &body, "application/json")
    }

    fn api_bytes(
        &self,
        method: &str,
        body: &[u8],
        content_type: &str,
    ) -> Result<Value, AdapterFailure> {
        let mut response = self.token.with_bytes(|token| {
            let token = std::str::from_utf8(token)
                .map_err(|_| authentication("Telegram token is not UTF-8"))?;
            if !safe_bot_token(token.as_bytes()) {
                return Err(authentication("Telegram token is not URL-path safe"));
            }
            let url = format!("{}/bot{token}/{method}", self.config.api_base);
            self.http
                .post(&url)
                .header("Content-Type", content_type)
                .send(body)
                .map_err(|error| transport_failure(&error))
        })?;
        let status = response.status().as_u16();
        let response_body = response
            .body_mut()
            .with_config()
            .limit(u64::try_from(self.config.max_event_bytes).unwrap_or(u64::MAX))
            .read_to_vec()
            .map_err(|error| transport_failure(&error))?;
        let parsed = serde_json::from_slice::<Value>(&response_body).ok();
        if !(200..=299).contains(&status) {
            return Err(classify_status(status, parsed.as_ref()));
        }
        let parsed = parsed.ok_or_else(|| malformed("Telegram returned malformed JSON"))?;
        if parsed.get("ok").and_then(Value::as_bool) != Some(true) {
            let error_code = parsed
                .get("error_code")
                .and_then(Value::as_u64)
                .and_then(|value| u16::try_from(value).ok())
                .unwrap_or(status);
            return Err(classify_status(error_code, Some(&parsed)));
        }
        parsed
            .get("result")
            .cloned()
            .ok_or_else(|| malformed("Telegram response has no result"))
    }
}

impl ChannelAdapter for TelegramAdapter {
    fn features(&self) -> AdapterFeatures {
        AdapterFeatures {
            capabilities: BTreeSet::from([
                AdapterCapability::Attachments,
                AdapterCapability::Threads,
                AdapterCapability::Steering,
                AdapterCapability::Cancellation,
                AdapterCapability::Reconnect,
            ]),
            max_attachment_bytes: self.config.max_attachment_bytes,
            requests_per_minute: None,
        }
    }

    fn receive(&mut self) -> Result<AdapterEvent, AdapterFailure> {
        if let Some(event) = self.pending.pop_front() {
            return Ok(event);
        }
        self.poll_once()?;
        self.pending
            .pop_front()
            .ok_or_else(|| retryable("Telegram returned no actionable update"))
    }

    fn send(&mut self, message: &OutboundMessage) -> Result<SendReceipt, AdapterFailure> {
        self.send_message(message)
    }

    fn reconnect(&mut self) -> Result<(), AdapterFailure> {
        self.api_call("getMe", &json!({}))?;
        Ok(())
    }
}

impl ChannelAdapterV2 for TelegramAdapter {
    fn capabilities_v2(&self) -> ChannelCapabilitiesV2 {
        telegram_capabilities(&self.config)
    }

    fn receive_v2(&mut self) -> Result<ChannelEventV2, ChannelAdapterErrorV2> {
        if let Some(event) = self.pending_v2.pop_front() {
            return Ok(event);
        }
        self.poll_once().map_err(adapter_failure_to_v2)?;
        self.pending_v2.pop_front().ok_or_else(|| {
            v2_error(
                ChannelAdapterErrorKindV2::TransientNetwork,
                "Telegram returned no actionable update",
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
            ChannelOperationV2::EditMessage {
                route,
                platform_message_id,
                text,
                rich_content,
            } => {
                if !rich_content.is_empty() {
                    return Err(ChannelAdapterErrorV2::unsupported(
                        "Telegram rich content translation is not implemented",
                    ));
                }
                let receipt = self
                    .edit_message(route, platform_message_id, text)
                    .map_err(adapter_failure_to_v2)?;
                Ok(operation_receipt(
                    format!("edit:{platform_message_id}"),
                    Some(receipt.platform_message_id),
                    ChannelReceiptStateV2::Accepted,
                    receipt.duplicate_possible,
                ))
            }
            ChannelOperationV2::SetTyping { route, active } => {
                self.validate_route(route).map_err(adapter_failure_to_v2)?;
                let mut receipt = operation_receipt(
                    format!("typing:{}", route.conversation),
                    None,
                    ChannelReceiptStateV2::Accepted,
                    false,
                );
                if *active {
                    self.send_typing(route).map_err(adapter_failure_to_v2)?;
                } else {
                    receipt.metadata.insert(
                        "telegram_typing".to_owned(),
                        "expires_automatically".to_owned(),
                    );
                }
                Ok(receipt)
            }
            ChannelOperationV2::Cancel { cancellation_id } => {
                if cancellation_id.trim().is_empty() {
                    return Err(ChannelAdapterErrorV2::malformed(
                        "Telegram cancellation identity is empty",
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
            ChannelOperationV2::DeleteMessage { .. }
            | ChannelOperationV2::AddReaction { .. }
            | ChannelOperationV2::RemoveReaction { .. } => Err(ChannelAdapterErrorV2::unsupported(
                "Telegram operation is not supported",
            )),
        }
    }

    fn reconnect_v2(&mut self) -> Result<(), ChannelAdapterErrorV2> {
        self.reconnect().map_err(adapter_failure_to_v2)
    }

    fn reconnect_cursor_v2(&self) -> Option<ReconnectCursorV2> {
        self.cursor
            .next_update_id
            .zip(self.cursor.last_update_at)
            .map(|(next_update_id, observed_at)| ReconnectCursorV2 {
                value: next_update_id.to_string(),
                observed_at,
            })
    }
}

impl TelegramAdapter {
    fn send_v2_message(
        &mut self,
        message: &ChannelOutboundMessageV2,
    ) -> Result<ChannelOperationReceiptV2, ChannelAdapterErrorV2> {
        if message.idempotency_key.trim().is_empty() {
            return Err(ChannelAdapterErrorV2::malformed(
                "Telegram delivery identity is empty",
            ));
        }
        if self.cancelled.contains(&message.idempotency_key) {
            return Err(v2_error(
                ChannelAdapterErrorKindV2::Cancelled,
                "Telegram delivery was cancelled before dispatch",
            ));
        }
        if !message.rich_content.is_empty() {
            return Err(ChannelAdapterErrorV2::unsupported(
                "Telegram rich content translation is not implemented",
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

#[allow(clippy::too_many_lines)]
fn telegram_event_v2(
    update_id: i64,
    update_kind: &str,
    raw_message: &Value,
    message: &InboundMessage,
) -> ChannelEventV2 {
    let chat_type = raw_message
        .pointer("/chat/type")
        .and_then(Value::as_str)
        .unwrap_or("private");
    let conversation = ChannelConversationV2 {
        platform_id: message.conversation.clone(),
        kind: match chat_type {
            "private" => ChannelConversationKindV2::Direct,
            "channel" => ChannelConversationKindV2::Channel,
            _ => ChannelConversationKindV2::GroupDirect,
        },
        thread_id: message.thread.clone(),
        reply_to_message_id: message.reply_target.clone(),
    };
    let sender_value = raw_message
        .get("from")
        .or_else(|| raw_message.get("sender_chat"));
    let display_name = sender_value.and_then(|sender| {
        let parts = [
            sender.get("first_name").and_then(Value::as_str),
            sender.get("last_name").and_then(Value::as_str),
        ]
        .into_iter()
        .flatten()
        .filter(|part| !part.is_empty())
        .collect::<Vec<_>>();
        if parts.is_empty() {
            sender
                .get("username")
                .and_then(Value::as_str)
                .map(str::to_owned)
        } else {
            Some(parts.join(" "))
        }
    });
    let sender = ChannelIdentityV2 {
        platform_id: message.sender.clone(),
        display_name,
        is_bot: sender_value
            .and_then(|sender| sender.get("is_bot"))
            .and_then(Value::as_bool)
            .unwrap_or(false),
    };
    let attachments = message
        .attachments
        .iter()
        .map(|attachment| {
            let kind = if raw_message.get("voice").is_some() {
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
            let duration_ms = raw_message
                .get(match kind {
                    ChannelAttachmentKindV2::Voice => "voice",
                    ChannelAttachmentKindV2::Audio => "audio",
                    ChannelAttachmentKindV2::Video => "video",
                    _ => "",
                })
                .and_then(|media| media.get("duration"))
                .and_then(Value::as_u64)
                .and_then(|seconds| seconds.checked_mul(1_000));
            ChannelAttachmentV2 {
                attachment: attachment.clone(),
                kind,
                duration_ms,
                metadata: BTreeMap::new(),
            }
        })
        .collect::<Vec<_>>();
    let mut metadata = BTreeMap::new();
    metadata.insert("telegram_update_id".to_owned(), update_id.to_string());
    metadata.insert("telegram_update_kind".to_owned(), update_kind.to_owned());
    let event = if update_kind.starts_with("edited_") {
        ChannelEventKindV2::MessageEdited(ChannelMessageEditV2 {
            message_id: message.message_id.clone(),
            account_id: message.external_account.clone(),
            conversation,
            editor: sender,
            text: message.text.clone(),
            occurred_at: message.occurred_at,
            metadata,
        })
    } else if let Some((name, arguments)) = parse_command(&message.text) {
        ChannelEventKindV2::Command(ChannelCommandV2 {
            command_id: format!("telegram:{}:{}", message.conversation, message.message_id),
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
        event_id: format!("telegram:{update_id}"),
        delivery_attempt: 1,
        event,
        metadata: BTreeMap::new(),
    }
}

fn telegram_attachments(
    message: &Value,
    max_attachment_bytes: u64,
) -> Result<Vec<Attachment>, AdapterFailure> {
    let mut attachments = Vec::new();
    if let Some(photo) = message
        .get("photo")
        .and_then(Value::as_array)
        .and_then(|sizes| {
            sizes
                .iter()
                .max_by_key(|item| item.get("file_size").and_then(Value::as_u64))
        })
    {
        attachments.push(telegram_attachment(
            photo,
            "telegram-photo",
            "image/jpeg",
            max_attachment_bytes,
        )?);
    }
    for (field, default_name, default_media_type) in [
        ("document", "telegram-document", "application/octet-stream"),
        ("audio", "telegram-audio", "audio/mpeg"),
        ("voice", "telegram-voice.ogg", "audio/ogg"),
        ("video", "telegram-video.mp4", "video/mp4"),
        ("sticker", "telegram-sticker.webp", "image/webp"),
    ] {
        if let Some(media) = message.get(field) {
            attachments.push(telegram_attachment(
                media,
                default_name,
                default_media_type,
                max_attachment_bytes,
            )?);
        }
    }
    Ok(attachments)
}

fn telegram_attachment(
    media: &Value,
    default_name: &str,
    default_media_type: &str,
    max_attachment_bytes: u64,
) -> Result<Attachment, AdapterFailure> {
    let byte_length = media.get("file_size").and_then(Value::as_u64).unwrap_or(0);
    if byte_length > max_attachment_bytes {
        return Err(permanent("Telegram inbound attachment is oversized"));
    }
    Ok(Attachment {
        id: string_field(media, "file_id")?,
        file_name: media
            .get("file_name")
            .and_then(Value::as_str)
            .unwrap_or(default_name)
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
        sha256: None,
    })
}

fn build_media_multipart(
    message: &OutboundMessage,
    upload: &TelegramUpload,
    field: &str,
) -> Result<(Vec<u8>, String), AdapterFailure> {
    let boundary = "keith-telegram-boundary";
    let mut body = Vec::new();
    multipart_text(&mut body, boundary, "chat_id", &message.route.conversation)?;
    if !message.text.is_empty() {
        multipart_text(&mut body, boundary, "caption", &message.text)?;
    }
    if let Some(thread) = &message.route.thread {
        thread
            .parse::<i64>()
            .map_err(|_| malformed("Telegram thread identity is malformed"))?;
        multipart_text(&mut body, boundary, "message_thread_id", thread)?;
    }
    if let Some(reply) = &message.route.reply_to_message {
        let reply = reply
            .parse::<i64>()
            .map_err(|_| malformed("Telegram reply identity is malformed"))?;
        multipart_text(
            &mut body,
            boundary,
            "reply_parameters",
            &json!({"message_id": reply}).to_string(),
        )?;
    }
    write!(
        body,
        "--{boundary}\r\nContent-Disposition: form-data; name=\"{field}\"; filename=\"{}\"\r\nContent-Type: {}\r\n\r\n",
        upload.file_name, upload.media_type
    )
    .map_err(|_| malformed("Telegram multipart payload could not be encoded"))?;
    body.extend_from_slice(&upload.bytes);
    body.extend_from_slice(b"\r\n");
    write!(body, "--{boundary}--\r\n")
        .map_err(|_| malformed("Telegram multipart payload could not be encoded"))?;
    Ok((body, format!("multipart/form-data; boundary={boundary}")))
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
    .map_err(|_| malformed("Telegram multipart payload could not be encoded"))
}

fn add_thread(payload: &mut Value, route: &ReplyRoute) -> Result<(), AdapterFailure> {
    if let Some(thread) = &route.thread {
        payload["message_thread_id"] = json!(
            thread
                .parse::<i64>()
                .map_err(|_| malformed("Telegram thread identity is malformed"))?
        );
    }
    Ok(())
}

fn add_reply(payload: &mut Value, route: &ReplyRoute) -> Result<(), AdapterFailure> {
    if let Some(reply) = &route.reply_to_message {
        let reply = reply
            .parse::<i64>()
            .map_err(|_| malformed("Telegram reply identity is malformed"))?;
        payload["reply_parameters"] = json!({"message_id": reply});
    }
    Ok(())
}

fn receipt_from_message(
    message: &Value,
    duplicate_possible: bool,
) -> Result<SendReceipt, AdapterFailure> {
    Ok(SendReceipt {
        platform_message_id: json_identity(message.get("message_id"))?,
        accepted_at: now(),
        duplicate_possible,
    })
}

fn command_intent(text: &str) -> InboundIntent {
    let command = text
        .split_ascii_whitespace()
        .next()
        .unwrap_or_default()
        .split('@')
        .next()
        .unwrap_or_default();
    match command {
        "/cancel" | "/stop" => InboundIntent::Cancel,
        "/steer" => InboundIntent::Steer,
        _ => InboundIntent::Prompt,
    }
}

fn parse_command(text: &str) -> Option<(String, String)> {
    let mut parts = text.splitn(2, char::is_whitespace);
    let command = parts.next()?.strip_prefix('/')?;
    let name = command.split('@').next()?.trim();
    if name.is_empty() {
        return None;
    }
    Some((
        name.to_owned(),
        parts.next().unwrap_or_default().trim().to_owned(),
    ))
}

fn json_identity(value: Option<&Value>) -> Result<String, AdapterFailure> {
    match value {
        Some(Value::String(value)) if !value.is_empty() => Ok(value.clone()),
        Some(Value::Number(value)) => Ok(value.to_string()),
        _ => Err(malformed("Telegram payload is missing an identity field")),
    }
}

fn string_field(value: &Value, field: &str) -> Result<String, AdapterFailure> {
    value
        .get(field)
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty())
        .map(str::to_owned)
        .ok_or_else(|| malformed("Telegram payload is missing an identity field"))
}

fn safe_file_path(path: &str) -> bool {
    !path.is_empty()
        && !path.starts_with('/')
        && !path.contains("..")
        && !path.contains(['\r', '\n', '\0'])
}

fn safe_bot_token(token: &[u8]) -> bool {
    !token.is_empty()
        && token.len() <= 256
        && token
            .iter()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b':' | b'_' | b'-'))
}

fn secret_matches(expected: &[u8], presented: &[u8]) -> bool {
    constant_time_eq(expected, presented)
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

fn telegram_capabilities(config: &TelegramConfig) -> ChannelCapabilitiesV2 {
    let unsupported = |safe_reason: &str| ChannelCapabilitySupportV2::Unsupported {
        safe_reason: safe_reason.to_owned(),
    };
    let mut declarations = ChannelCapabilityV2::ALL
        .into_iter()
        .map(|capability| {
            (
                capability,
                unsupported("Telegram adapter does not implement this capability"),
            )
        })
        .collect::<BTreeMap<_, _>>();
    for capability in [
        ChannelCapabilityV2::InboundMessages,
        ChannelCapabilityV2::OutboundMessages,
        ChannelCapabilityV2::Threads,
        ChannelCapabilityV2::Replies,
        ChannelCapabilityV2::Commands,
        ChannelCapabilityV2::MessageEdits,
        ChannelCapabilityV2::Attachments,
        ChannelCapabilityV2::Voice,
        ChannelCapabilityV2::Typing,
        ChannelCapabilityV2::RateLimits,
        ChannelCapabilityV2::Reconnect,
        ChannelCapabilityV2::Cancellation,
    ] {
        declarations.insert(capability, ChannelCapabilitySupportV2::Supported);
    }
    declarations.insert(
        ChannelCapabilityV2::Mentions,
        unsupported("Telegram mention entities are not normalized by this adapter"),
    );
    declarations.insert(
        ChannelCapabilityV2::MessageDeletion,
        unsupported("Telegram Bot API does not deliver general message deletion events"),
    );
    declarations.insert(
        ChannelCapabilityV2::Reactions,
        unsupported("Telegram reaction updates are not enabled by this adapter"),
    );
    declarations.insert(
        ChannelCapabilityV2::RichContent,
        unsupported("Telegram rich content translation is not implemented"),
    );
    declarations.insert(
        ChannelCapabilityV2::DeliveryReceipts,
        unsupported("Telegram Bot API does not expose delivery receipts"),
    );
    declarations.insert(
        ChannelCapabilityV2::ReadReceipts,
        unsupported("Telegram Bot API does not expose read receipts"),
    );
    declarations.insert(
        ChannelCapabilityV2::IdempotentSend,
        unsupported("Telegram Bot API does not accept an idempotency key"),
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
        ChannelEventKindV2::MessageEdited(message) => Some(message.occurred_at),
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
                || message.contains("another account") =>
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

fn classify_status(status: u16, body: Option<&Value>) -> AdapterFailure {
    match status {
        401 => authentication("Telegram authentication failed"),
        403 => permission("Telegram permission was denied"),
        429 => AdapterFailure {
            class: RetryClass::RateLimited,
            safe_message: "Telegram rate limit reached".to_owned(),
            retry_after_ms: body
                .and_then(|value| value.pointer("/parameters/retry_after"))
                .and_then(Value::as_u64)
                .and_then(|seconds| seconds.checked_mul(1_000)),
        },
        500..=599 => retryable("Telegram service is temporarily unavailable"),
        400..=499 => permanent("Telegram rejected the request"),
        _ => retryable("Telegram returned an unexpected response"),
    }
}

fn transport_failure(error: &ureq::Error) -> AdapterFailure {
    match error {
        ureq::Error::Timeout(_) => retryable("Telegram request timed out"),
        _ => reconnect_failure("Telegram transport failed"),
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
    fn telegram_api_error_taxonomy_preserves_rate_limit_and_revocation() {
        let rate_limit = classify_status(429, Some(&json!({"parameters": {"retry_after": 4}})));
        assert_eq!(rate_limit.class, RetryClass::RateLimited);
        assert_eq!(rate_limit.retry_after_ms, Some(4_000));
        let revoked = adapter_failure_to_v2(classify_status(401, None));
        assert_eq!(revoked.kind, ChannelAdapterErrorKindV2::Authentication);
        let denied = adapter_failure_to_v2(classify_status(403, None));
        assert_eq!(denied.kind, ChannelAdapterErrorKindV2::Permission);
    }

    #[test]
    fn telegram_media_request_is_bounded_and_contains_no_bot_token() {
        let message = OutboundMessage {
            route: ReplyRoute {
                channel: TELEGRAM_CHANNEL.to_owned(),
                external_account: "bot-account".to_owned(),
                conversation: "42".to_owned(),
                thread: Some("7".to_owned()),
                reply_to_message: Some("8".to_owned()),
            },
            idempotency_key: "outbox-1".to_owned(),
            text: "caption".to_owned(),
            artifacts: Vec::new(),
        };
        let upload = TelegramUpload {
            kind: TelegramUploadKind::Voice,
            file_name: "voice.ogg".to_owned(),
            media_type: "audio/ogg".to_owned(),
            bytes: b"voice-bytes".to_vec(),
        };
        let (body, content_type) =
            build_media_multipart(&message, &upload, "voice").expect("multipart request");
        let body = String::from_utf8(body).expect("test multipart is UTF-8");
        assert!(body.contains("message_thread_id"));
        assert!(body.contains("reply_parameters"));
        assert!(body.contains("voice-bytes"));
        assert!(!body.contains("bot-token"));
        assert!(content_type.starts_with("multipart/form-data; boundary="));
    }
}
