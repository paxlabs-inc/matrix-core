use std::collections::{BTreeMap, BTreeSet, VecDeque};
use std::net::TcpStream;
use std::time::Duration;

use keith_agent_types::{ArtifactId, UtcTimestamp};
use keith_channel_core::{
    AdapterCapability, AdapterEvent, AdapterFailure, AdapterFeatures, Attachment,
    CHANNEL_CONTRACT_V2, ChannelAccountSetupV2, ChannelAdapter, ChannelAdapterErrorKindV2,
    ChannelAdapterErrorV2, ChannelAdapterV2, ChannelAttachmentKindV2, ChannelAttachmentV2,
    ChannelCapabilitiesV2, ChannelCapabilitySupportV2, ChannelCapabilityV2, ChannelCommandV2,
    ChannelConformanceV2, ChannelConnectionHealthV2, ChannelContractVersion,
    ChannelConversationKindV2, ChannelConversationV2, ChannelEventKindV2, ChannelEventV2,
    ChannelIdentityV2, ChannelMentionV2, ChannelMessageDeleteV2, ChannelMessageEditV2,
    ChannelMessageV2, ChannelOperationReceiptV2, ChannelOperationV2, ChannelOutboundMessageV2,
    ChannelReactionActionV2, ChannelReactionV2, ChannelReceiptStateV2, ChannelRichContentV2,
    InboundIntent, InboundMessage, OutboundMessage, RawWebhookRequestV2, ReconnectCursorV2,
    ReplyRoute, RetryClass, SendReceipt, VerifiedWebhookRequestV2,
};
use keith_credentials::SecretValue;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tungstenite::stream::MaybeTlsStream;
use tungstenite::{Message, WebSocket, connect};

const SLACK_WEBHOOK_MAX_SKEW_SECONDS: i64 = 300;

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SlackConfig {
    pub api_base: String,
    pub team_id: String,
    pub bot_user_id: String,
    pub webhook_enabled: bool,
    pub max_event_bytes: usize,
    pub max_attachment_bytes: u64,
    pub max_attachments: usize,
    pub max_staged_bytes: usize,
    pub max_rich_content_bytes: usize,
    pub timeout_ms: u64,
    pub deduplication_capacity: usize,
    pub requests_per_minute: u32,
}

impl SlackConfig {
    pub fn production(team_id: impl Into<String>, bot_user_id: impl Into<String>) -> Self {
        Self {
            api_base: "https://slack.com/api".to_owned(),
            team_id: team_id.into(),
            bot_user_id: bot_user_id.into(),
            webhook_enabled: true,
            max_event_bytes: 1024 * 1_024,
            max_attachment_bytes: 1024 * 1_024 * 1_024,
            max_attachments: 10,
            max_staged_bytes: 2 * 1024 * 1_024 * 1_024,
            max_rich_content_bytes: 256 * 1_024,
            timeout_ms: 30_000,
            deduplication_capacity: 4_096,
            requests_per_minute: 50,
        }
    }
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SlackCursor {
    pub recent_event_ids: Vec<String>,
    pub last_envelope_id: Option<String>,
    pub last_event_at: Option<UtcTimestamp>,
    pub reconnect_count: u64,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SlackUpload {
    pub file_name: String,
    pub media_type: String,
    pub bytes: Vec<u8>,
    pub title: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum SlackWebhookOutcome {
    Challenge(String),
    Event(Option<Box<ChannelEventV2>>),
}

type SlackSocket = WebSocket<MaybeTlsStream<TcpStream>>;

pub struct SlackAdapter {
    config: SlackConfig,
    bot_token: SecretValue,
    app_token: Option<SecretValue>,
    signing_secret: SecretValue,
    http: ureq::Agent,
    socket: Option<SlackSocket>,
    cursor: SlackCursor,
    seen: BTreeSet<String>,
    seen_order: VecDeque<String>,
    staged: BTreeMap<ArtifactId, SlackUpload>,
    staged_bytes: usize,
    cancelled: BTreeSet<String>,
}

impl SlackAdapter {
    /// Creates a profile-account-scoped Slack adapter without exposing credential values.
    ///
    /// # Errors
    ///
    /// Returns a permanent configuration error for invalid identities, endpoints, or bounds.
    pub fn new(
        config: SlackConfig,
        bot_token: SecretValue,
        app_token: Option<SecretValue>,
        signing_secret: SecretValue,
        cursor: SlackCursor,
    ) -> Result<Self, ChannelAdapterErrorV2> {
        if config.team_id.trim().is_empty()
            || config.bot_user_id.trim().is_empty()
            || config.max_event_bytes == 0
            || config.max_attachment_bytes == 0
            || config.max_attachments == 0
            || config.max_staged_bytes == 0
            || config.max_rich_content_bytes == 0
            || config.timeout_ms == 0
            || config.deduplication_capacity == 0
            || config.requests_per_minute == 0
            || (!config.webhook_enabled && app_token.is_none())
            || !valid_http_base(&config.api_base)
        {
            return Err(v2_error(
                ChannelAdapterErrorKindV2::PermanentDestination,
                "invalid Slack adapter configuration",
            ));
        }
        let http: ureq::Agent = ureq::Agent::config_builder()
            .timeout_global(Some(Duration::from_millis(config.timeout_ms)))
            .http_status_as_error(false)
            .build()
            .into();
        let seen_order = cursor
            .recent_event_ids
            .iter()
            .rev()
            .take(config.deduplication_capacity)
            .cloned()
            .collect::<Vec<_>>()
            .into_iter()
            .rev()
            .collect::<VecDeque<_>>();
        let seen = seen_order.iter().cloned().collect();
        let adapter = Self {
            config,
            bot_token,
            app_token,
            signing_secret,
            http,
            socket: None,
            cursor,
            seen,
            seen_order,
            staged: BTreeMap::new(),
            staged_bytes: 0,
            cancelled: BTreeSet::new(),
        };
        adapter.capabilities_v2().validate()?;
        Ok(adapter)
    }

    pub const fn cursor(&self) -> &SlackCursor {
        &self.cursor
    }

    pub fn setup_diagnostics(&self) -> ChannelAccountSetupV2 {
        let mut required_credential_names =
            BTreeSet::from(["bot_token".to_owned(), "signing_secret".to_owned()]);
        if self.app_token.is_some() {
            required_credential_names.insert("app_token".to_owned());
        }
        ChannelAccountSetupV2 {
            account_id: self.config.team_id.clone(),
            required_credential_names,
            required_scopes: BTreeSet::from([
                "app_mentions:read".to_owned(),
                "channels:history".to_owned(),
                "chat:write".to_owned(),
                "commands".to_owned(),
                "files:read".to_owned(),
                "files:write".to_owned(),
                "reactions:read".to_owned(),
                "reactions:write".to_owned(),
            ]),
            webhook_configured: self.config.webhook_enabled,
            socket_or_polling_configured: self.app_token.is_some(),
            connection_health: if self.socket.is_some() {
                ChannelConnectionHealthV2::Connected
            } else {
                ChannelConnectionHealthV2::Disconnected
            },
            reconnect_cursor_present: !self.cursor.recent_event_ids.is_empty(),
            safe_test_supported: true,
            metadata: slack_metadata(&self.config.team_id),
        }
    }

    /// Runs Slack's read-only authentication test and verifies exact account isolation.
    ///
    /// # Errors
    ///
    /// Returns a classified authentication, permission, transport, or account-mismatch error.
    pub fn test_connection(&self) -> Result<(), ChannelAdapterErrorV2> {
        let response = self.api_json_bot("auth.test", &json!({}), false)?;
        let team_id = required_string(&response, "team_id", "Slack auth test")?;
        let user_id = required_string(&response, "user_id", "Slack auth test")?;
        if team_id != self.config.team_id || user_id != self.config.bot_user_id {
            return Err(v2_error(
                ChannelAdapterErrorKindV2::Permission,
                "Slack authentication resolved to a different workspace account",
            ));
        }
        Ok(())
    }

    /// Stages bounded artifact bytes for Slack's external upload flow.
    ///
    /// # Errors
    ///
    /// Returns a permanent error for malformed metadata or exceeded byte budgets.
    pub fn stage_artifact(
        &mut self,
        id: ArtifactId,
        upload: SlackUpload,
    ) -> Result<(), ChannelAdapterErrorV2> {
        if upload.file_name.trim().is_empty()
            || upload.media_type.trim().is_empty()
            || upload.bytes.is_empty()
            || u64::try_from(upload.bytes.len()).unwrap_or(u64::MAX)
                > self.config.max_attachment_bytes
            || upload.file_name.contains(['\r', '\n'])
            || upload.media_type.contains(['\r', '\n'])
        {
            return Err(v2_error(
                ChannelAdapterErrorKindV2::PermanentDestination,
                "Slack attachment is invalid or oversized",
            ));
        }
        let old = self.staged.get(&id).map_or(0, |value| value.bytes.len());
        let next = self
            .staged_bytes
            .saturating_sub(old)
            .saturating_add(upload.bytes.len());
        if next > self.config.max_staged_bytes {
            return Err(v2_error(
                ChannelAdapterErrorKindV2::PermanentDestination,
                "Slack staged attachment budget exceeded",
            ));
        }
        self.staged.insert(id, upload);
        self.staged_bytes = next;
        Ok(())
    }

    /// Verifies a Slack HTTP signature against the raw bytes before decoding an Events API,
    /// URL-verification, or slash-command body.
    ///
    /// # Errors
    ///
    /// Returns authentication before parsing for stale or invalid signatures, then a malformed
    /// event error for invalid JSON or form data.
    pub fn ingest_webhook(
        &mut self,
        timestamp_seconds: i64,
        signature: &str,
        body: &[u8],
        now_seconds: i64,
    ) -> Result<SlackWebhookOutcome, ChannelAdapterErrorV2> {
        if body.len() > self.config.max_event_bytes {
            return Err(ChannelAdapterErrorV2::malformed(
                "Slack webhook exceeded its configured byte bound",
            ));
        }
        let verified = self.signing_secret.with_bytes(|secret| {
            VerifiedWebhookRequestV2::verify(
                RawWebhookRequestV2 {
                    timestamp_seconds,
                    signature,
                    body,
                },
                now_seconds,
                SLACK_WEBHOOK_MAX_SKEW_SECONDS,
                |timestamp, raw_body, presented| {
                    verify_slack_signature(secret, timestamp, raw_body, presented)
                },
            )
        })?;
        if verified.body().first() == Some(&b'{') {
            let payload: Value = serde_json::from_slice(verified.body())
                .map_err(|_| ChannelAdapterErrorV2::malformed("Slack webhook JSON is malformed"))?;
            if payload.get("type").and_then(Value::as_str) == Some("url_verification") {
                return required_string(&payload, "challenge", "Slack URL verification")
                    .map(SlackWebhookOutcome::Challenge);
            }
            return self
                .normalize_events_api(&payload)
                .map(|event| SlackWebhookOutcome::Event(event.map(Box::new)));
        }
        let form = parse_form(verified.body())?;
        self.normalize_slash_command(&form, None)
            .map(|event| SlackWebhookOutcome::Event(event.map(Box::new)))
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

    fn connect_socket(&mut self) -> Result<(), ChannelAdapterErrorV2> {
        let app_token = self.app_token.as_ref().ok_or_else(|| {
            v2_error(
                ChannelAdapterErrorKindV2::Authentication,
                "Slack Socket Mode requires an app-level token",
            )
        })?;
        let response =
            self.api_json_with_token(app_token, "apps.connections.open", &json!({}), false)?;
        let url = required_string(&response, "url", "Slack Socket Mode response")?;
        if !valid_websocket_url(&url) {
            return Err(ChannelAdapterErrorV2::malformed(
                "Slack Socket Mode returned an invalid WebSocket URL",
            ));
        }
        let (socket, _) = connect(url.as_str()).map_err(|_| {
            v2_error(
                ChannelAdapterErrorKindV2::TransientNetwork,
                "Slack Socket Mode connection failed",
            )
        })?;
        self.socket = Some(socket);
        self.cursor.reconnect_count = self.cursor.reconnect_count.saturating_add(1);
        Ok(())
    }

    fn receive_socket_event(&mut self) -> Result<ChannelEventV2, ChannelAdapterErrorV2> {
        if self.socket.is_none() {
            self.connect_socket()?;
        }
        loop {
            let payload = read_socket_json(
                self.socket.as_mut().expect("Slack socket connected"),
                self.config.max_event_bytes,
            )?;
            if payload.get("type").and_then(Value::as_str) == Some("disconnect") {
                self.socket = None;
                return Ok(ChannelEventV2 {
                    contract: CHANNEL_CONTRACT_V2,
                    event_id: format!(
                        "slack-disconnect-{}",
                        self.cursor.reconnect_count.saturating_add(1)
                    ),
                    delivery_attempt: 1,
                    event: ChannelEventKindV2::ReconnectRequired {
                        safe_reason: "Slack requested a Socket Mode refresh".to_owned(),
                    },
                    metadata: slack_metadata(&self.config.team_id),
                });
            }
            let envelope_id = required_string(&payload, "envelope_id", "Slack Socket envelope")?;
            let envelope_type = required_string(&payload, "type", "Slack Socket envelope")?;
            let normalized = match envelope_type.as_str() {
                "events_api" => {
                    let inner = payload.get("payload").ok_or_else(|| {
                        ChannelAdapterErrorV2::malformed("Slack Socket event has no payload")
                    })?;
                    self.normalize_events_api(inner)?
                }
                "slash_commands" => {
                    let inner = payload.get("payload").ok_or_else(|| {
                        ChannelAdapterErrorV2::malformed("Slack command has no payload")
                    })?;
                    self.normalize_slash_command_value(inner, Some(&envelope_id))?
                }
                _ => None,
            };
            self.acknowledge(&envelope_id)?;
            self.cursor.last_envelope_id = Some(envelope_id.clone());
            if let Some(mut event) = normalized {
                event
                    .metadata
                    .insert("slack_envelope_id".to_owned(), envelope_id);
                return Ok(event);
            }
        }
    }

    fn acknowledge(&mut self, envelope_id: &str) -> Result<(), ChannelAdapterErrorV2> {
        self.socket
            .as_mut()
            .ok_or_else(|| {
                v2_error(
                    ChannelAdapterErrorKindV2::TransientNetwork,
                    "Slack Socket Mode disconnected before acknowledgement",
                )
            })?
            .send(Message::Text(
                json!({"envelope_id": envelope_id}).to_string().into(),
            ))
            .map_err(|_| {
                v2_error(
                    ChannelAdapterErrorKindV2::UncertainAcknowledgement,
                    "Slack Socket Mode acknowledgement was uncertain",
                )
            })
    }

    fn normalize_events_api(
        &mut self,
        payload: &Value,
    ) -> Result<Option<ChannelEventV2>, ChannelAdapterErrorV2> {
        if payload.get("type").and_then(Value::as_str) != Some("event_callback") {
            return Err(ChannelAdapterErrorV2::malformed(
                "Slack Events API wrapper type is unsupported",
            ));
        }
        let team_id = required_string(payload, "team_id", "Slack Events API wrapper")?;
        if team_id != self.config.team_id {
            return Err(v2_error(
                ChannelAdapterErrorKindV2::Permission,
                "Slack event belongs to a different workspace account",
            ));
        }
        let event_id = required_string(payload, "event_id", "Slack Events API wrapper")?;
        if self.seen.contains(&event_id) {
            return Ok(None);
        }
        let event = payload
            .get("event")
            .ok_or_else(|| ChannelAdapterErrorV2::malformed("Slack event payload is absent"))?;
        let occurred_at = slack_event_time(payload, event);
        let normalized = match event.get("type").and_then(Value::as_str) {
            Some("message" | "app_mention") => self.normalize_message(event)?,
            Some("reaction_added") => Some(ChannelEventKindV2::Reaction(
                self.normalize_reaction(event, ChannelReactionActionV2::Added)?,
            )),
            Some("reaction_removed") => Some(ChannelEventKindV2::Reaction(
                self.normalize_reaction(event, ChannelReactionActionV2::Removed)?,
            )),
            _ => None,
        };
        self.remember(event_id.clone(), occurred_at);
        let Some(event) = normalized else {
            return Ok(None);
        };
        let mut metadata = slack_metadata(&self.config.team_id);
        if let Some(context) = payload.get("event_context").and_then(Value::as_str) {
            metadata.insert("slack_event_context".to_owned(), context.to_owned());
        }
        let normalized = ChannelEventV2 {
            contract: CHANNEL_CONTRACT_V2,
            event_id,
            delivery_attempt: 1,
            event,
            metadata,
        };
        ChannelConformanceV2::admit_event(&self.capabilities_v2(), &normalized)?;
        Ok(Some(normalized))
    }

    fn normalize_message(
        &self,
        event: &Value,
    ) -> Result<Option<ChannelEventKindV2>, ChannelAdapterErrorV2> {
        match event.get("subtype").and_then(Value::as_str) {
            Some("message_changed") => {
                let message = event.get("message").ok_or_else(|| {
                    ChannelAdapterErrorV2::malformed("Slack changed message is absent")
                })?;
                let editor = slack_identity(
                    message
                        .get("user")
                        .or_else(|| event.get("user"))
                        .and_then(Value::as_str)
                        .unwrap_or("unknown"),
                    message.get("bot_id").is_some(),
                );
                if editor.platform_id == self.config.bot_user_id || editor.is_bot {
                    return Ok(None);
                }
                let conversation = slack_conversation(event, message)?;
                let edit = ChannelMessageEditV2 {
                    message_id: required_string(message, "ts", "Slack changed message")?,
                    account_id: self.config.team_id.clone(),
                    conversation,
                    editor,
                    text: message
                        .get("text")
                        .and_then(Value::as_str)
                        .unwrap_or_default()
                        .to_owned(),
                    occurred_at: slack_event_time(event, message),
                    metadata: slack_metadata(&self.config.team_id),
                };
                Ok(Some(ChannelEventKindV2::MessageEdited(edit)))
            }
            Some("message_deleted") => {
                let conversation = slack_conversation(event, event)?;
                let deletion = ChannelMessageDeleteV2 {
                    message_id: required_string(event, "deleted_ts", "Slack deleted message")?,
                    account_id: self.config.team_id.clone(),
                    conversation,
                    actor: event
                        .get("user")
                        .and_then(Value::as_str)
                        .map(|user| slack_identity(user, false)),
                    occurred_at: slack_event_time(event, event),
                    metadata: slack_metadata(&self.config.team_id),
                };
                Ok(Some(ChannelEventKindV2::MessageDeleted(deletion)))
            }
            Some(subtype)
                if !matches!(
                    subtype,
                    "file_share" | "thread_broadcast" | "reply_broadcast"
                ) =>
            {
                Ok(None)
            }
            _ => {
                let sender_id = required_string(event, "user", "Slack message")?;
                if sender_id == self.config.bot_user_id || event.get("bot_id").is_some() {
                    return Ok(None);
                }
                let text = event
                    .get("text")
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_owned();
                let attachments = self.slack_attachments(event)?;
                if text.trim().is_empty() && attachments.is_empty() {
                    return Err(ChannelAdapterErrorV2::malformed(
                        "Slack message has no text or bounded attachment",
                    ));
                }
                let rich_content = self.slack_rich_content(event)?;
                let message = ChannelMessageV2 {
                    message_id: event
                        .get("client_msg_id")
                        .and_then(Value::as_str)
                        .or_else(|| event.get("ts").and_then(Value::as_str))
                        .ok_or_else(|| {
                            ChannelAdapterErrorV2::malformed("Slack message ID is absent")
                        })?
                        .to_owned(),
                    account_id: self.config.team_id.clone(),
                    conversation: slack_conversation(event, event)?,
                    sender: slack_identity(&sender_id, false),
                    mentions: slack_mentions(&text),
                    text,
                    attachments,
                    rich_content,
                    occurred_at: slack_event_time(event, event),
                    metadata: slack_metadata(&self.config.team_id),
                };
                Ok(Some(ChannelEventKindV2::MessageCreated(message)))
            }
        }
    }

    fn normalize_reaction(
        &self,
        event: &Value,
        action: ChannelReactionActionV2,
    ) -> Result<ChannelReactionV2, ChannelAdapterErrorV2> {
        let item = event
            .get("item")
            .ok_or_else(|| ChannelAdapterErrorV2::malformed("Slack reaction item is absent"))?;
        if item.get("type").and_then(Value::as_str) != Some("message") {
            return Err(ChannelAdapterErrorV2::unsupported(
                "Slack reaction does not target a message",
            ));
        }
        let channel = required_string(item, "channel", "Slack reaction item")?;
        Ok(ChannelReactionV2 {
            message_id: required_string(item, "ts", "Slack reaction item")?,
            account_id: self.config.team_id.clone(),
            conversation: ChannelConversationV2 {
                kind: slack_conversation_kind(&channel),
                platform_id: channel,
                thread_id: None,
                reply_to_message_id: None,
            },
            actor: slack_identity(&required_string(event, "user", "Slack reaction")?, false),
            reaction: required_string(event, "reaction", "Slack reaction")?,
            action,
            occurred_at: slack_event_time(event, event),
            metadata: slack_metadata(&self.config.team_id),
        })
    }

    fn normalize_slash_command_value(
        &mut self,
        payload: &Value,
        envelope_id: Option<&str>,
    ) -> Result<Option<ChannelEventV2>, ChannelAdapterErrorV2> {
        let form = payload
            .as_object()
            .ok_or_else(|| ChannelAdapterErrorV2::malformed("Slack command is malformed"))?
            .iter()
            .filter_map(|(key, value)| value.as_str().map(|value| (key.clone(), value.to_owned())))
            .collect::<BTreeMap<_, _>>();
        self.normalize_slash_command(&form, envelope_id)
    }

    fn normalize_slash_command(
        &mut self,
        form: &BTreeMap<String, String>,
        envelope_id: Option<&str>,
    ) -> Result<Option<ChannelEventV2>, ChannelAdapterErrorV2> {
        let team_id = form_value(form, "team_id", "Slack command")?;
        if team_id != self.config.team_id {
            return Err(v2_error(
                ChannelAdapterErrorKindV2::Permission,
                "Slack command belongs to a different workspace account",
            ));
        }
        let trigger_id = form_value(form, "trigger_id", "Slack command")?;
        let event_id = envelope_id.unwrap_or(&trigger_id).to_owned();
        let occurred_at = UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH);
        if self.seen.contains(&event_id) {
            return Ok(None);
        }
        let channel = form_value(form, "channel_id", "Slack command")?;
        let command = ChannelCommandV2 {
            command_id: trigger_id,
            account_id: self.config.team_id.clone(),
            conversation: ChannelConversationV2 {
                kind: slack_conversation_kind(&channel),
                platform_id: channel,
                thread_id: None,
                reply_to_message_id: None,
            },
            sender: slack_identity(&form_value(form, "user_id", "Slack command")?, false),
            name: form_value(form, "command", "Slack command")?,
            arguments: form.get("text").cloned().unwrap_or_default(),
            occurred_at,
            metadata: slack_metadata(&self.config.team_id),
        };
        self.remember(event_id.clone(), occurred_at);
        let event = ChannelEventV2 {
            contract: CHANNEL_CONTRACT_V2,
            event_id,
            delivery_attempt: 1,
            event: ChannelEventKindV2::Command(command),
            metadata: slack_metadata(&self.config.team_id),
        };
        ChannelConformanceV2::admit_event(&self.capabilities_v2(), &event)?;
        Ok(Some(event))
    }

    fn slack_attachments(
        &self,
        event: &Value,
    ) -> Result<Vec<ChannelAttachmentV2>, ChannelAdapterErrorV2> {
        let files = event
            .get("files")
            .and_then(Value::as_array)
            .cloned()
            .unwrap_or_default();
        if files.len() > self.config.max_attachments {
            return Err(ChannelAdapterErrorV2::malformed(
                "Slack message has too many attachments",
            ));
        }
        files
            .iter()
            .map(|file| {
                let byte_length = file
                    .get("size")
                    .and_then(Value::as_u64)
                    .ok_or_else(|| ChannelAdapterErrorV2::malformed("Slack file size is absent"))?;
                if byte_length > self.config.max_attachment_bytes {
                    return Err(ChannelAdapterErrorV2::malformed(
                        "Slack attachment exceeds its configured byte bound",
                    ));
                }
                let media_type = file
                    .get("mimetype")
                    .and_then(Value::as_str)
                    .unwrap_or("application/octet-stream")
                    .to_owned();
                let kind = attachment_kind(&media_type);
                Ok(ChannelAttachmentV2 {
                    attachment: Attachment {
                        id: required_string(file, "id", "Slack file")?,
                        file_name: file
                            .get("name")
                            .or_else(|| file.get("title"))
                            .and_then(Value::as_str)
                            .unwrap_or("attachment")
                            .to_owned(),
                        media_type,
                        byte_length,
                        artifact_id: None,
                        download_url: file
                            .get("url_private_download")
                            .or_else(|| file.get("url_private"))
                            .and_then(Value::as_str)
                            .map(str::to_owned),
                        staging_file: None,
                        sha256: None,
                    },
                    kind,
                    duration_ms: file.get("duration_ms").and_then(Value::as_u64),
                    metadata: BTreeMap::new(),
                })
            })
            .collect()
    }

    fn slack_rich_content(
        &self,
        event: &Value,
    ) -> Result<Vec<ChannelRichContentV2>, ChannelAdapterErrorV2> {
        let blocks = event
            .get("blocks")
            .and_then(Value::as_array)
            .cloned()
            .unwrap_or_default();
        let mut total = 0_usize;
        blocks
            .iter()
            .map(|block| {
                let encoded = serde_json::to_string(block).map_err(|_| {
                    ChannelAdapterErrorV2::malformed("Slack rich content cannot be encoded")
                })?;
                total = total.saturating_add(encoded.len());
                if total > self.config.max_rich_content_bytes {
                    return Err(ChannelAdapterErrorV2::malformed(
                        "Slack rich content exceeds its configured byte bound",
                    ));
                }
                let mut metadata = BTreeMap::new();
                metadata.insert("slack_block_json".to_owned(), encoded);
                Ok(ChannelRichContentV2 {
                    kind: block
                        .get("type")
                        .and_then(Value::as_str)
                        .unwrap_or("unknown")
                        .to_owned(),
                    text: block
                        .get("text")
                        .and_then(|text| text.get("text"))
                        .and_then(Value::as_str)
                        .unwrap_or_default()
                        .to_owned(),
                    metadata,
                })
            })
            .collect()
    }

    fn execute_operation(
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
                self.validate_route(route)?;
                let response = self.api_json_bot(
                    "chat.update",
                    &json!({
                        "channel": route.conversation,
                        "ts": platform_message_id,
                        "text": text,
                        "blocks": rich_content_blocks(rich_content)?,
                    }),
                    true,
                )?;
                Ok(operation_receipt(
                    format!("edit:{platform_message_id}"),
                    response.get("ts").and_then(Value::as_str),
                    ChannelReceiptStateV2::Accepted,
                ))
            }
            ChannelOperationV2::DeleteMessage {
                route,
                platform_message_id,
            } => {
                self.validate_route(route)?;
                let response = self.api_json_bot(
                    "chat.delete",
                    &json!({"channel": route.conversation, "ts": platform_message_id}),
                    true,
                )?;
                Ok(operation_receipt(
                    format!("delete:{platform_message_id}"),
                    response.get("ts").and_then(Value::as_str),
                    ChannelReceiptStateV2::Accepted,
                ))
            }
            ChannelOperationV2::AddReaction {
                route,
                platform_message_id,
                reaction,
            }
            | ChannelOperationV2::RemoveReaction {
                route,
                platform_message_id,
                reaction,
            } => {
                self.validate_route(route)?;
                if reaction.trim().is_empty() {
                    return Err(v2_error(
                        ChannelAdapterErrorKindV2::PermanentDestination,
                        "Slack reaction name is empty",
                    ));
                }
                let method = if matches!(operation, ChannelOperationV2::AddReaction { .. }) {
                    "reactions.add"
                } else {
                    "reactions.remove"
                };
                self.api_json_bot(
                    method,
                    &json!({
                        "channel": route.conversation,
                        "timestamp": platform_message_id,
                        "name": reaction,
                    }),
                    true,
                )?;
                Ok(operation_receipt(
                    format!("{method}:{platform_message_id}:{reaction}"),
                    Some(platform_message_id),
                    ChannelReceiptStateV2::Accepted,
                ))
            }
            ChannelOperationV2::SetTyping { .. } => Err(ChannelAdapterErrorV2::unsupported(
                "Slack does not expose bot typing state",
            )),
            ChannelOperationV2::Cancel { cancellation_id } => {
                if cancellation_id.trim().is_empty() {
                    return Err(ChannelAdapterErrorV2::malformed(
                        "Slack cancellation identity is empty",
                    ));
                }
                self.cancelled.insert(cancellation_id.clone());
                Ok(operation_receipt(
                    cancellation_id.clone(),
                    None,
                    ChannelReceiptStateV2::Cancelled,
                ))
            }
        }
    }

    fn send_v2_message(
        &mut self,
        message: &ChannelOutboundMessageV2,
    ) -> Result<ChannelOperationReceiptV2, ChannelAdapterErrorV2> {
        self.validate_route(&message.route)?;
        if message.idempotency_key.trim().is_empty() {
            return Err(ChannelAdapterErrorV2::malformed(
                "Slack delivery identity is empty",
            ));
        }
        if message.text.chars().count() > 40_000 {
            return Err(v2_error(
                ChannelAdapterErrorKindV2::PermanentDestination,
                "Slack message exceeds the platform text bound",
            ));
        }
        if self.cancelled.contains(&message.idempotency_key) {
            return Err(v2_error(
                ChannelAdapterErrorKindV2::Cancelled,
                "Slack delivery was cancelled before dispatch",
            ));
        }
        if message.artifacts.len() > self.config.max_attachments {
            return Err(v2_error(
                ChannelAdapterErrorKindV2::PermanentDestination,
                "Slack delivery has too many artifacts",
            ));
        }
        if message.artifacts.is_empty() {
            let thread_ts = message
                .route
                .thread
                .as_ref()
                .or(message.route.reply_to_message.as_ref());
            let response = self.api_json_bot(
                "chat.postMessage",
                &json!({
                    "channel": message.route.conversation,
                    "thread_ts": thread_ts,
                    "text": message.text,
                    "blocks": rich_content_blocks(&message.rich_content)?,
                    "client_msg_id": message.idempotency_key,
                }),
                true,
            )?;
            let platform_message_id = required_string(&response, "ts", "Slack send receipt")?;
            let mut receipt = operation_receipt(
                message.idempotency_key.clone(),
                Some(&platform_message_id),
                ChannelReceiptStateV2::Accepted,
            );
            receipt.metadata.insert(
                "slack_channel".to_owned(),
                message.route.conversation.clone(),
            );
            return Ok(receipt);
        }
        if !message.rich_content.is_empty() {
            return Err(ChannelAdapterErrorV2::unsupported(
                "Slack external file completion cannot preserve message blocks",
            ));
        }
        self.upload_files(message)
    }

    fn upload_files(
        &mut self,
        message: &ChannelOutboundMessageV2,
    ) -> Result<ChannelOperationReceiptV2, ChannelAdapterErrorV2> {
        let mut completed = Vec::new();
        for artifact_id in &message.artifacts {
            let upload = self.staged.get(artifact_id).ok_or_else(|| {
                v2_error(
                    ChannelAdapterErrorKindV2::PermanentDestination,
                    "Slack artifact bytes were not staged",
                )
            })?;
            let response = self.api_json_bot(
                "files.getUploadURLExternal",
                &json!({"filename": upload.file_name, "length": upload.bytes.len()}),
                false,
            )?;
            let upload_url = required_string(&response, "upload_url", "Slack upload URL")?;
            let file_id = required_string(&response, "file_id", "Slack upload URL")?;
            self.upload_bytes(&upload_url, upload)?;
            completed.push(json!({
                "id": file_id,
                "title": upload.title.as_deref().unwrap_or(&upload.file_name),
            }));
        }
        let response = self.api_json_bot(
            "files.completeUploadExternal",
            &json!({
                "files": completed,
                "channel_id": message.route.conversation,
                "thread_ts": message.route.thread.as_ref().or(message.route.reply_to_message.as_ref()),
                "initial_comment": message.text,
            }),
            true,
        )?;
        let platform_id = response
            .get("files")
            .and_then(Value::as_array)
            .and_then(|files| files.first())
            .and_then(|file| file.get("id"))
            .and_then(Value::as_str)
            .ok_or_else(|| ChannelAdapterErrorV2::malformed("Slack file receipt has no file ID"))?
            .to_owned();
        for artifact_id in &message.artifacts {
            if let Some(upload) = self.staged.remove(artifact_id) {
                self.staged_bytes = self.staged_bytes.saturating_sub(upload.bytes.len());
            }
        }
        Ok(operation_receipt(
            message.idempotency_key.clone(),
            Some(&platform_id),
            ChannelReceiptStateV2::Accepted,
        ))
    }

    fn upload_bytes(
        &self,
        upload_url: &str,
        upload: &SlackUpload,
    ) -> Result<(), ChannelAdapterErrorV2> {
        if !valid_http_base(upload_url) {
            return Err(ChannelAdapterErrorV2::malformed(
                "Slack returned an invalid external upload URL",
            ));
        }
        let response = self
            .http
            .post(upload_url)
            .header("Content-Type", &upload.media_type)
            .send(&upload.bytes)
            .map_err(|_| {
                v2_error(
                    ChannelAdapterErrorKindV2::UncertainAcknowledgement,
                    "Slack external file upload acknowledgement was uncertain",
                )
            })?;
        if response.status().is_success() {
            Ok(())
        } else {
            Err(v2_error(
                ChannelAdapterErrorKindV2::PermanentDestination,
                "Slack external file upload was rejected",
            ))
        }
    }

    fn validate_route(&self, route: &ReplyRoute) -> Result<(), ChannelAdapterErrorV2> {
        if route.channel != "slack"
            || route.external_account != self.config.team_id
            || route.conversation.trim().is_empty()
        {
            return Err(v2_error(
                ChannelAdapterErrorKindV2::Permission,
                "Slack delivery route does not belong to this workspace account",
            ));
        }
        Ok(())
    }

    fn api_json_bot(
        &self,
        method: &str,
        payload: &Value,
        side_effecting: bool,
    ) -> Result<Value, ChannelAdapterErrorV2> {
        self.api_json_with_token(&self.bot_token, method, payload, side_effecting)
    }

    fn api_json_with_token(
        &self,
        token: &SecretValue,
        method: &str,
        payload: &Value,
        side_effecting: bool,
    ) -> Result<Value, ChannelAdapterErrorV2> {
        let body = serde_json::to_vec(payload)
            .map_err(|_| ChannelAdapterErrorV2::malformed("Slack API request cannot be encoded"))?;
        let url = format!("{}/{method}", self.config.api_base.trim_end_matches('/'));
        let response = token.with_bytes(|token| {
            let token = std::str::from_utf8(token).map_err(|_| {
                v2_error(
                    ChannelAdapterErrorKindV2::Authentication,
                    "Slack credential is not UTF-8",
                )
            })?;
            self.http
                .post(&url)
                .header("Authorization", format!("Bearer {token}"))
                .header("Content-Type", "application/json; charset=utf-8")
                .send(&body)
                .map_err(|_| {
                    v2_error(
                        if side_effecting {
                            ChannelAdapterErrorKindV2::UncertainAcknowledgement
                        } else {
                            ChannelAdapterErrorKindV2::TransientNetwork
                        },
                        if side_effecting {
                            "Slack API acknowledgement was uncertain"
                        } else {
                            "Slack API request failed"
                        },
                    )
                })
        })?;
        let status = response.status().as_u16();
        let retry_after_ms = response
            .headers()
            .get("retry-after")
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.parse::<u64>().ok())
            .map(|seconds| seconds.saturating_mul(1_000));
        let mut response = response;
        let response_body = response
            .body_mut()
            .with_config()
            .limit(u64::try_from(self.config.max_event_bytes).unwrap_or(u64::MAX))
            .read_to_vec()
            .map_err(|_| {
                v2_error(
                    if side_effecting {
                        ChannelAdapterErrorKindV2::UncertainAcknowledgement
                    } else {
                        ChannelAdapterErrorKindV2::TransientNetwork
                    },
                    "Slack API response could not be read",
                )
            })?;
        if status == 429 {
            return Err(ChannelAdapterErrorV2 {
                kind: ChannelAdapterErrorKindV2::RateLimit,
                safe_message: "Slack API rate limit reached".to_owned(),
                retry_after_ms,
            });
        }
        if matches!(status, 401 | 403) {
            return Err(v2_error(
                if status == 401 {
                    ChannelAdapterErrorKindV2::Authentication
                } else {
                    ChannelAdapterErrorKindV2::Permission
                },
                "Slack API authentication or permission was denied",
            ));
        }
        if status >= 500 {
            return Err(v2_error(
                ChannelAdapterErrorKindV2::TransientNetwork,
                "Slack API is temporarily unavailable",
            ));
        }
        if !(200..300).contains(&status) {
            return Err(v2_error(
                ChannelAdapterErrorKindV2::PermanentDestination,
                "Slack API rejected the request",
            ));
        }
        let decoded: Value = serde_json::from_slice(&response_body)
            .map_err(|_| ChannelAdapterErrorV2::malformed("Slack API returned malformed JSON"))?;
        if decoded.get("ok").and_then(Value::as_bool) == Some(true) {
            return Ok(decoded);
        }
        Err(classify_slack_api_error(
            decoded
                .get("error")
                .and_then(Value::as_str)
                .unwrap_or("unknown_error"),
            retry_after_ms,
        ))
    }
}

impl ChannelAdapterV2 for SlackAdapter {
    fn capabilities_v2(&self) -> ChannelCapabilitiesV2 {
        slack_capabilities(&self.config)
    }

    fn receive_v2(&mut self) -> Result<ChannelEventV2, ChannelAdapterErrorV2> {
        self.receive_socket_event()
    }

    fn execute_v2(
        &mut self,
        operation: &ChannelOperationV2,
    ) -> Result<ChannelOperationReceiptV2, ChannelAdapterErrorV2> {
        self.execute_operation(operation)
    }

    fn reconnect_v2(&mut self) -> Result<(), ChannelAdapterErrorV2> {
        self.socket = None;
        self.connect_socket()
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

impl ChannelAdapter for SlackAdapter {
    fn features(&self) -> AdapterFeatures {
        AdapterFeatures {
            capabilities: BTreeSet::from([
                AdapterCapability::Attachments,
                AdapterCapability::Threads,
                AdapterCapability::Steering,
                AdapterCapability::Cancellation,
                AdapterCapability::IdempotentSend,
                AdapterCapability::Reconnect,
            ]),
            max_attachment_bytes: self.config.max_attachment_bytes,
            requests_per_minute: Some(self.config.requests_per_minute),
        }
    }

    fn receive(&mut self) -> Result<AdapterEvent, AdapterFailure> {
        loop {
            let event = self.receive_v2().map_err(AdapterFailure::from_channel_v2)?;
            if let Some(event) = v2_to_v1(event) {
                return Ok(event);
            }
        }
    }

    fn send(&mut self, message: &OutboundMessage) -> Result<SendReceipt, AdapterFailure> {
        let operation = ChannelOperationV2::SendMessage(ChannelOutboundMessageV2 {
            route: message.route.clone(),
            idempotency_key: message.idempotency_key.clone(),
            text: message.text.clone(),
            artifacts: message.artifacts.clone(),
            rich_content: Vec::new(),
            metadata: BTreeMap::new(),
        });
        let receipt = self
            .execute_v2(&operation)
            .map_err(AdapterFailure::from_channel_v2)?;
        Ok(SendReceipt {
            platform_message_id: receipt
                .platform_message_id
                .unwrap_or_else(|| receipt.operation_id.clone()),
            accepted_at: receipt.accepted_at,
            duplicate_possible: receipt.duplicate_possible,
        })
    }

    fn reconnect(&mut self) -> Result<(), AdapterFailure> {
        self.reconnect_v2().map_err(AdapterFailure::from_channel_v2)
    }
}

trait AdapterFailureV2Ext {
    fn from_channel_v2(error: ChannelAdapterErrorV2) -> Self;
}

impl AdapterFailureV2Ext for AdapterFailure {
    fn from_channel_v2(error: ChannelAdapterErrorV2) -> Self {
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
        Self {
            class,
            safe_message: error.safe_message,
            retry_after_ms: error.retry_after_ms,
        }
    }
}

fn slack_capabilities(config: &SlackConfig) -> ChannelCapabilitiesV2 {
    let unsupported = |safe_reason: &str| ChannelCapabilitySupportV2::Unsupported {
        safe_reason: safe_reason.to_owned(),
    };
    let mut declarations = ChannelCapabilityV2::ALL
        .into_iter()
        .map(|capability| (capability, ChannelCapabilitySupportV2::Supported))
        .collect::<BTreeMap<_, _>>();
    declarations.insert(
        ChannelCapabilityV2::Voice,
        unsupported("Slack bot applications receive audio as files, not native voice messages"),
    );
    declarations.insert(
        ChannelCapabilityV2::Typing,
        unsupported("Slack does not expose bot typing state"),
    );
    declarations.insert(
        ChannelCapabilityV2::ReadReceipts,
        unsupported("Slack does not expose per-message read receipts to bot applications"),
    );
    ChannelCapabilitiesV2 {
        contract: ChannelContractVersion::new(2, 0),
        declarations,
        max_event_bytes: u64::try_from(config.max_event_bytes).unwrap_or(u64::MAX),
        max_attachment_bytes: config.max_attachment_bytes,
        max_attachments: config.max_attachments,
        max_rich_content_bytes: u64::try_from(config.max_rich_content_bytes).unwrap_or(u64::MAX),
        requests_per_minute: Some(config.requests_per_minute),
    }
}

fn read_socket_json(
    socket: &mut SlackSocket,
    max_event_bytes: usize,
) -> Result<Value, ChannelAdapterErrorV2> {
    loop {
        let message = socket.read().map_err(|_| {
            v2_error(
                ChannelAdapterErrorKindV2::TransientNetwork,
                "Slack Socket Mode transport failed",
            )
        })?;
        let bytes = match message {
            Message::Text(text) => text.as_bytes().to_vec(),
            Message::Binary(bytes) => bytes.to_vec(),
            Message::Ping(bytes) => {
                socket.send(Message::Pong(bytes)).map_err(|_| {
                    v2_error(
                        ChannelAdapterErrorKindV2::TransientNetwork,
                        "Slack Socket Mode pong failed",
                    )
                })?;
                continue;
            }
            Message::Pong(_) | Message::Frame(_) => continue,
            Message::Close(_) => {
                return Err(v2_error(
                    ChannelAdapterErrorKindV2::TransientNetwork,
                    "Slack Socket Mode closed",
                ));
            }
        };
        if bytes.len() > max_event_bytes {
            return Err(ChannelAdapterErrorV2::malformed(
                "Slack Socket Mode event exceeded its configured byte bound",
            ));
        }
        return serde_json::from_slice(&bytes)
            .map_err(|_| ChannelAdapterErrorV2::malformed("Slack Socket Mode event is malformed"));
    }
}

fn v2_to_v1(event: ChannelEventV2) -> Option<AdapterEvent> {
    let event_id = event.event_id;
    match event.event {
        ChannelEventKindV2::MessageCreated(message) => {
            let thread = message.conversation.thread_id.clone();
            Some(AdapterEvent::Inbound(Box::new(InboundMessage {
                channel: "slack".to_owned(),
                external_account: message.account_id,
                conversation: message.conversation.platform_id,
                thread,
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
        ChannelEventKindV2::Command(command) => {
            Some(AdapterEvent::Inbound(Box::new(InboundMessage {
                channel: "slack".to_owned(),
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
        ChannelEventKindV2::MessageEdited(edit) => {
            Some(AdapterEvent::Inbound(Box::new(InboundMessage {
                channel: "slack".to_owned(),
                external_account: edit.account_id,
                conversation: edit.conversation.platform_id,
                thread: edit.conversation.thread_id,
                sender: edit.editor.platform_id,
                message_id: event_id,
                reply_target: Some(edit.message_id),
                text: format!("[Slack message edited]\n{}", edit.text),
                attachments: Vec::new(),
                occurred_at: edit.occurred_at,
                intent: InboundIntent::Prompt,
            })))
        }
        ChannelEventKindV2::MessageDeleted(deletion) => {
            Some(AdapterEvent::Inbound(Box::new(InboundMessage {
                channel: "slack".to_owned(),
                external_account: deletion.account_id,
                conversation: deletion.conversation.platform_id,
                thread: deletion.conversation.thread_id,
                sender: deletion
                    .actor
                    .map_or_else(|| "unknown".to_owned(), |actor| actor.platform_id),
                message_id: event_id,
                reply_target: Some(deletion.message_id.clone()),
                text: format!("[Slack message deleted: {}]", deletion.message_id),
                attachments: Vec::new(),
                occurred_at: deletion.occurred_at,
                intent: InboundIntent::Prompt,
            })))
        }
        ChannelEventKindV2::Reaction(reaction) => {
            let action = match reaction.action {
                ChannelReactionActionV2::Added => "added",
                ChannelReactionActionV2::Removed => "removed",
            };
            Some(AdapterEvent::Inbound(Box::new(InboundMessage {
                channel: "slack".to_owned(),
                external_account: reaction.account_id,
                conversation: reaction.conversation.platform_id,
                thread: reaction.conversation.thread_id,
                sender: reaction.actor.platform_id,
                message_id: event_id,
                reply_target: Some(reaction.message_id),
                text: format!("[Slack reaction {action}: {}]", reaction.reaction),
                attachments: Vec::new(),
                occurred_at: reaction.occurred_at,
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

fn operation_receipt(
    operation_id: String,
    platform_message_id: Option<&str>,
    state: ChannelReceiptStateV2,
) -> ChannelOperationReceiptV2 {
    ChannelOperationReceiptV2 {
        operation_id,
        platform_message_id: platform_message_id.map(str::to_owned),
        accepted_at: UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH),
        state,
        duplicate_possible: false,
        metadata: BTreeMap::new(),
    }
}

fn classify_slack_api_error(error: &str, retry_after_ms: Option<u64>) -> ChannelAdapterErrorV2 {
    let kind = match error {
        "invalid_auth" | "not_authed" | "token_revoked" | "account_inactive" => {
            ChannelAdapterErrorKindV2::Authentication
        }
        "missing_scope" | "not_allowed_token_type" | "no_permission" | "not_in_channel" => {
            ChannelAdapterErrorKindV2::Permission
        }
        "ratelimited" => ChannelAdapterErrorKindV2::RateLimit,
        "internal_error" | "fatal_error" | "service_unavailable" => {
            ChannelAdapterErrorKindV2::TransientNetwork
        }
        _ => ChannelAdapterErrorKindV2::PermanentDestination,
    };
    ChannelAdapterErrorV2 {
        kind,
        safe_message: format!("Slack API rejected the operation: {error}"),
        retry_after_ms: (kind == ChannelAdapterErrorKindV2::RateLimit)
            .then_some(retry_after_ms)
            .flatten(),
    }
}

fn rich_content_blocks(
    rich_content: &[ChannelRichContentV2],
) -> Result<Value, ChannelAdapterErrorV2> {
    rich_content
        .iter()
        .map(|block| {
            let encoded = block.metadata.get("slack_block_json").ok_or_else(|| {
                ChannelAdapterErrorV2::unsupported(
                    "Slack rich content requires a preserved Slack block payload",
                )
            })?;
            serde_json::from_str(encoded)
                .map_err(|_| ChannelAdapterErrorV2::malformed("Slack block payload is malformed"))
        })
        .collect::<Result<Vec<Value>, _>>()
        .map(Value::Array)
}

fn slack_conversation(
    outer: &Value,
    message: &Value,
) -> Result<ChannelConversationV2, ChannelAdapterErrorV2> {
    let channel = outer
        .get("channel")
        .or_else(|| message.get("channel"))
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty())
        .ok_or_else(|| ChannelAdapterErrorV2::malformed("Slack conversation is absent"))?
        .to_owned();
    let message_id = message.get("ts").and_then(Value::as_str);
    let thread_id = message
        .get("thread_ts")
        .and_then(Value::as_str)
        .map(str::to_owned);
    let reply_to_message_id = thread_id
        .as_deref()
        .filter(|thread| Some(*thread) != message_id)
        .map(str::to_owned);
    Ok(ChannelConversationV2 {
        kind: slack_conversation_kind(&channel),
        platform_id: channel,
        thread_id,
        reply_to_message_id,
    })
}

fn slack_conversation_kind(channel: &str) -> ChannelConversationKindV2 {
    match channel.as_bytes().first() {
        Some(b'D') => ChannelConversationKindV2::Direct,
        Some(b'G') => ChannelConversationKindV2::GroupDirect,
        _ => ChannelConversationKindV2::Channel,
    }
}

fn slack_identity(id: &str, is_bot: bool) -> ChannelIdentityV2 {
    ChannelIdentityV2 {
        platform_id: id.to_owned(),
        display_name: None,
        is_bot,
    }
}

fn slack_mentions(text: &str) -> Vec<ChannelMentionV2> {
    let mut mentions = Vec::new();
    let mut offset = 0;
    while let Some(relative_start) = text[offset..].find("<@") {
        let start = offset + relative_start;
        let identity_start = start + 2;
        let Some(relative_end) = text[identity_start..].find('>') else {
            break;
        };
        let end = identity_start + relative_end;
        let id = text[identity_start..end]
            .split_once('|')
            .map_or(&text[identity_start..end], |(id, _)| id);
        if !id.is_empty() {
            mentions.push(ChannelMentionV2 {
                identity: slack_identity(id, false),
                start: Some(start),
                end: Some(end + 1),
            });
        }
        offset = end + 1;
    }
    mentions
}

fn slack_event_time(wrapper: &Value, event: &Value) -> UtcTimestamp {
    wrapper
        .get("event_time")
        .and_then(Value::as_i64)
        .and_then(|seconds| seconds.checked_mul(1_000))
        .map(UtcTimestamp::from_unix_millis)
        .or_else(|| {
            event
                .get("event_ts")
                .or_else(|| event.get("ts"))
                .and_then(Value::as_str)
                .and_then(slack_timestamp)
        })
        .unwrap_or_else(|| UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH))
}

fn slack_timestamp(value: &str) -> Option<UtcTimestamp> {
    let (seconds, fraction) = value.split_once('.').unwrap_or((value, "0"));
    let seconds = seconds.parse::<i64>().ok()?;
    let milliseconds = fraction
        .chars()
        .take(3)
        .collect::<String>()
        .parse::<i64>()
        .ok()
        .unwrap_or(0);
    seconds
        .checked_mul(1_000)
        .and_then(|base| base.checked_add(milliseconds))
        .map(UtcTimestamp::from_unix_millis)
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

fn slack_metadata(team_id: &str) -> BTreeMap<String, String> {
    BTreeMap::from([
        ("platform".to_owned(), "slack".to_owned()),
        ("slack_team_id".to_owned(), team_id.to_owned()),
    ])
}

fn required_string(
    value: &Value,
    field: &str,
    context: &str,
) -> Result<String, ChannelAdapterErrorV2> {
    value
        .get(field)
        .and_then(Value::as_str)
        .filter(|value| !value.trim().is_empty())
        .map(str::to_owned)
        .ok_or_else(|| ChannelAdapterErrorV2::malformed(format!("{context} is missing {field}")))
}

fn form_value(
    form: &BTreeMap<String, String>,
    field: &str,
    context: &str,
) -> Result<String, ChannelAdapterErrorV2> {
    form.get(field)
        .filter(|value| !value.trim().is_empty())
        .cloned()
        .ok_or_else(|| ChannelAdapterErrorV2::malformed(format!("{context} is missing {field}")))
}

fn parse_form(body: &[u8]) -> Result<BTreeMap<String, String>, ChannelAdapterErrorV2> {
    let body = std::str::from_utf8(body)
        .map_err(|_| ChannelAdapterErrorV2::malformed("Slack command body is not UTF-8"))?;
    body.split('&')
        .map(|pair| {
            let (key, value) = pair.split_once('=').unwrap_or((pair, ""));
            Ok((percent_decode(key)?, percent_decode(value)?))
        })
        .collect()
}

fn percent_decode(value: &str) -> Result<String, ChannelAdapterErrorV2> {
    let bytes = value.as_bytes();
    let mut decoded = Vec::with_capacity(bytes.len());
    let mut index = 0;
    while index < bytes.len() {
        match bytes[index] {
            b'+' => decoded.push(b' '),
            b'%' if index + 2 < bytes.len() => {
                let high = hex_value(bytes[index + 1]).ok_or_else(|| {
                    ChannelAdapterErrorV2::malformed("Slack form escape is malformed")
                })?;
                let low = hex_value(bytes[index + 2]).ok_or_else(|| {
                    ChannelAdapterErrorV2::malformed("Slack form escape is malformed")
                })?;
                decoded.push((high << 4) | low);
                index += 2;
            }
            b'%' => {
                return Err(ChannelAdapterErrorV2::malformed(
                    "Slack form escape is truncated",
                ));
            }
            byte => decoded.push(byte),
        }
        index += 1;
    }
    String::from_utf8(decoded)
        .map_err(|_| ChannelAdapterErrorV2::malformed("Slack form value is not UTF-8"))
}

const fn hex_value(byte: u8) -> Option<u8> {
    match byte {
        b'0'..=b'9' => Some(byte - b'0'),
        b'a'..=b'f' => Some(byte - b'a' + 10),
        b'A'..=b'F' => Some(byte - b'A' + 10),
        _ => None,
    }
}

fn valid_http_base(value: &str) -> bool {
    value.starts_with("https://")
        || value.starts_with("http://127.0.0.1:")
        || value.starts_with("http://localhost:")
}

fn valid_websocket_url(value: &str) -> bool {
    value.starts_with("wss://")
        || value.starts_with("ws://127.0.0.1:")
        || value.starts_with("ws://localhost:")
}

fn v2_error(kind: ChannelAdapterErrorKindV2, message: &str) -> ChannelAdapterErrorV2 {
    ChannelAdapterErrorV2 {
        kind,
        safe_message: message.to_owned(),
        retry_after_ms: None,
    }
}

fn verify_slack_signature(
    signing_secret: &[u8],
    timestamp_seconds: i64,
    body: &[u8],
    presented: &str,
) -> bool {
    let mut base = format!("v0:{timestamp_seconds}:").into_bytes();
    base.extend_from_slice(body);
    let digest = hmac_sha256(signing_secret, &base);
    let expected = format!("v0={}", hex_encode(&digest));
    constant_time_eq(expected.as_bytes(), presented.as_bytes())
}

fn hmac_sha256(key: &[u8], message: &[u8]) -> [u8; 32] {
    let mut block = [0_u8; 64];
    if key.len() > block.len() {
        block[..32].copy_from_slice(&sha256(key));
    } else {
        block[..key.len()].copy_from_slice(key);
    }
    let mut inner_pad = [0x36_u8; 64];
    let mut outer_pad = [0x5c_u8; 64];
    for ((inner, outer), key_byte) in inner_pad.iter_mut().zip(outer_pad.iter_mut()).zip(block) {
        *inner ^= key_byte;
        *outer ^= key_byte;
    }
    let mut inner = Vec::with_capacity(inner_pad.len() + message.len());
    inner.extend_from_slice(&inner_pad);
    inner.extend_from_slice(message);
    let inner_hash = sha256(&inner);
    let mut outer = Vec::with_capacity(outer_pad.len() + inner_hash.len());
    outer.extend_from_slice(&outer_pad);
    outer.extend_from_slice(&inner_hash);
    sha256(&outer)
}

#[allow(
    clippy::many_single_char_names,
    clippy::needless_range_loop,
    clippy::unreadable_literal
)]
fn sha256(input: &[u8]) -> [u8; 32] {
    const INITIAL: [u32; 8] = [
        0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab,
        0x5be0cd19,
    ];
    const ROUND: [u32; 64] = [
        0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4,
        0xab1c5ed5, 0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe,
        0x9bdc06a7, 0xc19bf174, 0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f,
        0x4a7484aa, 0x5cb0a9dc, 0x76f988da, 0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7,
        0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967, 0x27b70a85, 0x2e1b2138, 0x4d2c6dfc,
        0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85, 0xa2bfe8a1, 0xa81a664b,
        0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070, 0x19a4c116,
        0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
        0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7,
        0xc67178f2,
    ];
    let bit_length = u64::try_from(input.len())
        .unwrap_or(u64::MAX)
        .saturating_mul(8);
    let mut padded = input.to_vec();
    padded.push(0x80);
    while padded.len() % 64 != 56 {
        padded.push(0);
    }
    padded.extend_from_slice(&bit_length.to_be_bytes());
    let mut state = INITIAL;
    for chunk in padded.chunks_exact(64) {
        let mut schedule = [0_u32; 64];
        for index in 0..16 {
            let offset = index * 4;
            schedule[index] = u32::from_be_bytes([
                chunk[offset],
                chunk[offset + 1],
                chunk[offset + 2],
                chunk[offset + 3],
            ]);
        }
        for index in 16..64 {
            let s0 = schedule[index - 15].rotate_right(7)
                ^ schedule[index - 15].rotate_right(18)
                ^ (schedule[index - 15] >> 3);
            let s1 = schedule[index - 2].rotate_right(17)
                ^ schedule[index - 2].rotate_right(19)
                ^ (schedule[index - 2] >> 10);
            schedule[index] = schedule[index - 16]
                .wrapping_add(s0)
                .wrapping_add(schedule[index - 7])
                .wrapping_add(s1);
        }
        let [mut a, mut b, mut c, mut d, mut e, mut f, mut g, mut h] = state;
        for index in 0..64 {
            let sum1 = e.rotate_right(6) ^ e.rotate_right(11) ^ e.rotate_right(25);
            let choose = (e & f) ^ ((!e) & g);
            let temp1 = h
                .wrapping_add(sum1)
                .wrapping_add(choose)
                .wrapping_add(ROUND[index])
                .wrapping_add(schedule[index]);
            let sum0 = a.rotate_right(2) ^ a.rotate_right(13) ^ a.rotate_right(22);
            let majority = (a & b) ^ (a & c) ^ (b & c);
            let temp2 = sum0.wrapping_add(majority);
            h = g;
            g = f;
            f = e;
            e = d.wrapping_add(temp1);
            d = c;
            c = b;
            b = a;
            a = temp1.wrapping_add(temp2);
        }
        state[0] = state[0].wrapping_add(a);
        state[1] = state[1].wrapping_add(b);
        state[2] = state[2].wrapping_add(c);
        state[3] = state[3].wrapping_add(d);
        state[4] = state[4].wrapping_add(e);
        state[5] = state[5].wrapping_add(f);
        state[6] = state[6].wrapping_add(g);
        state[7] = state[7].wrapping_add(h);
    }
    let mut digest = [0_u8; 32];
    for (chunk, value) in digest.chunks_exact_mut(4).zip(state) {
        chunk.copy_from_slice(&value.to_be_bytes());
    }
    digest
}

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
