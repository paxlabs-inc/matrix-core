#![forbid(unsafe_code)]

pub mod email;
pub mod google_chat;
pub mod matrix;
pub mod slack;
pub mod teams;
pub mod telegram;
pub mod whatsapp;

use std::collections::{BTreeMap, BTreeSet, VecDeque};
use std::io::{BufRead, BufReader, Read, Write};
use std::net::TcpStream;
use std::time::Duration;

use keith_agent_types::{ArtifactId, UtcTimestamp};
use keith_channel_core::{
    AdapterCapability, AdapterEvent, AdapterFailure, AdapterFeatures, Attachment,
    CHANNEL_CONTRACT_V2, ChannelAccountSetupV2, ChannelAdapter, ChannelAdapterErrorKindV2,
    ChannelAdapterErrorV2, ChannelAdapterV2, ChannelAttachmentKindV2, ChannelAttachmentV2,
    ChannelCapabilitiesV2, ChannelCapabilitySupportV2, ChannelCapabilityV2,
    ChannelConnectionHealthV2, ChannelConversationKindV2, ChannelConversationV2,
    ChannelEventKindV2, ChannelEventV2, ChannelIdentityV2, ChannelMentionV2, ChannelMessageV2,
    ChannelOperationReceiptV2, ChannelOperationV2, ChannelReceiptStateV2, InboundIntent,
    InboundMessage, OutboundMessage, ReconnectCursorV2, RetryClass, SendReceipt,
};
use keith_credentials::SecretValue;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tungstenite::stream::MaybeTlsStream;
use tungstenite::{Message, WebSocket, connect};

pub use email::{EmailAdapter, EmailConfig, EmailCursor};
pub use google_chat::{GoogleChatAdapter, GoogleChatConfig, GoogleChatCursor};
pub use matrix::{MatrixAdapter, MatrixConfig, MatrixCursor};
pub use slack::{SlackAdapter, SlackConfig, SlackCursor};
pub use teams::{TeamsAdapter, TeamsConfig, TeamsCursor};
pub use telegram::{TelegramAdapter, TelegramConfig, TelegramCursor, TelegramIngress};
pub use whatsapp::{WhatsAppCloudAdapter, WhatsAppCloudConfig, WhatsAppCursor};

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ChannelAdapterKind {
    Discord,
    Slack,
    Telegram,
    WhatsAppCloud,
    MicrosoftTeams,
    GoogleChat,
    Email,
    Matrix,
}

impl ChannelAdapterKind {
    pub const ALL: [Self; 8] = [
        Self::Discord,
        Self::Slack,
        Self::Telegram,
        Self::WhatsAppCloud,
        Self::MicrosoftTeams,
        Self::GoogleChat,
        Self::Email,
        Self::Matrix,
    ];

    pub const fn channel(self) -> &'static str {
        match self {
            Self::Discord => "discord",
            Self::Slack => "slack",
            Self::Telegram => "telegram",
            Self::WhatsAppCloud => "whatsapp_cloud",
            Self::MicrosoftTeams => "teams",
            Self::GoogleChat => "google_chat",
            Self::Email => "email",
            Self::Matrix => "matrix",
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelAdapterDefinition {
    pub kind: ChannelAdapterKind,
    pub channel: String,
    pub required_credential_names: BTreeSet<String>,
    pub required_scopes: BTreeSet<String>,
    pub webhook_supported: bool,
    pub socket_or_polling_supported: bool,
    pub safe_test_supported: bool,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelAdapterRegistration {
    pub kind: ChannelAdapterKind,
    pub account_id: String,
    pub capabilities: ChannelCapabilitiesV2,
    pub setup: ChannelAccountSetupV2,
}

pub trait ManagedChannelAdapter: ChannelAdapterV2 + Send {
    fn kind(&self) -> ChannelAdapterKind;
    fn account_setup(&self) -> ChannelAccountSetupV2;

    /// Performs the platform's read-only account continuity test.
    ///
    /// # Errors
    ///
    /// Returns a classified authentication, permission, transport, or account-mismatch error.
    fn test_connection(&self) -> Result<(), ChannelAdapterErrorV2>;
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ChannelAccountLifecycle {
    Starting,
    Running,
    Paused,
    RateLimited,
    Reconnecting,
    Failed,
    Removing,
    Removed,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelAccountHealth {
    pub kind: ChannelAdapterKind,
    pub account_id: String,
    pub lifecycle: ChannelAccountLifecycle,
    pub connection: ChannelConnectionHealthV2,
    pub queue_depth: usize,
    pub queue_capacity: usize,
    pub restart_count: u32,
    pub consecutive_failures: u32,
    pub throttled_until: Option<UtcTimestamp>,
    pub last_event_at: Option<UtcTimestamp>,
    pub last_delivery_at: Option<UtcTimestamp>,
    pub reconnect_cursor_present: bool,
    pub credential_generation: u64,
    pub safe_error: Option<String>,
}

impl ChannelAccountHealth {
    /// Validates a secret-free account health projection.
    ///
    /// # Errors
    ///
    /// Returns a malformed-contract error for blank identities, impossible queue bounds, or an
    /// inconsistent throttled state.
    pub fn validate(&self) -> Result<(), ChannelAdapterErrorV2> {
        if self.account_id.trim().is_empty()
            || self.queue_capacity == 0
            || self.queue_depth > self.queue_capacity
            || (self.lifecycle == ChannelAccountLifecycle::RateLimited
                && self.throttled_until.is_none())
            || self
                .safe_error
                .as_ref()
                .is_some_and(|value| value.trim().is_empty())
        {
            return Err(ChannelAdapterErrorV2::malformed(
                "channel account health is invalid",
            ));
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum ChannelCatalogError {
    UnknownKind,
    DuplicateAccount,
    MissingRequiredCredential(String),
    MissingRequiredScope(String),
    IngressMismatch,
    InvalidRegistration(ChannelAdapterErrorV2),
}

impl std::fmt::Display for ChannelCatalogError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::UnknownKind => formatter.write_str("channel adapter kind is not registered"),
            Self::DuplicateAccount => {
                formatter.write_str("channel adapter account is already registered")
            }
            Self::MissingRequiredCredential(name) => {
                write!(
                    formatter,
                    "channel adapter account is missing credential {name}"
                )
            }
            Self::MissingRequiredScope(scope) => {
                write!(
                    formatter,
                    "channel adapter account is missing scope {scope}"
                )
            }
            Self::IngressMismatch => formatter.write_str(
                "channel adapter account configured an ingress mode the catalog does not support",
            ),
            Self::InvalidRegistration(error) => formatter.write_str(&error.safe_message),
        }
    }
}

impl std::error::Error for ChannelCatalogError {}

pub struct ChannelAdapterCatalog {
    definitions: BTreeMap<ChannelAdapterKind, ChannelAdapterDefinition>,
    accounts: BTreeMap<(ChannelAdapterKind, String), ChannelAdapterRegistration>,
}

impl Default for ChannelAdapterCatalog {
    fn default() -> Self {
        Self::built_in()
    }
}

impl ChannelAdapterCatalog {
    pub fn built_in() -> Self {
        let definitions = ChannelAdapterKind::ALL
            .into_iter()
            .map(|kind| (kind, built_in_definition(kind)))
            .collect();
        Self {
            definitions,
            accounts: BTreeMap::new(),
        }
    }

    pub fn definitions(&self) -> impl ExactSizeIterator<Item = &ChannelAdapterDefinition> {
        self.definitions.values()
    }

    pub fn definition(&self, kind: ChannelAdapterKind) -> Option<&ChannelAdapterDefinition> {
        self.definitions.get(&kind)
    }

    /// Registers one real configured adapter through the shared managed interface.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid setup/capabilities, missing catalog requirements, or a
    /// duplicate account.
    pub fn register_adapter(
        &mut self,
        adapter: &dyn ManagedChannelAdapter,
    ) -> Result<&ChannelAdapterRegistration, ChannelCatalogError> {
        self.register_account(
            adapter.kind(),
            adapter.account_setup(),
            adapter.capabilities_v2(),
        )
    }

    /// Registers one configured account using the exact capabilities reported by its real
    /// adapter instance.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid capabilities/setup, missing required credential references,
    /// unknown adapter kinds, or duplicate account identities.
    pub fn register_account(
        &mut self,
        kind: ChannelAdapterKind,
        setup: ChannelAccountSetupV2,
        capabilities: ChannelCapabilitiesV2,
    ) -> Result<&ChannelAdapterRegistration, ChannelCatalogError> {
        let definition = self
            .definitions
            .get(&kind)
            .ok_or(ChannelCatalogError::UnknownKind)?;
        setup
            .validate()
            .map_err(ChannelCatalogError::InvalidRegistration)?;
        capabilities
            .validate()
            .map_err(ChannelCatalogError::InvalidRegistration)?;
        for required in &definition.required_credential_names {
            if !setup.required_credential_names.contains(required) {
                return Err(ChannelCatalogError::MissingRequiredCredential(
                    required.clone(),
                ));
            }
        }
        for required in &definition.required_scopes {
            if !setup.required_scopes.contains(required) {
                return Err(ChannelCatalogError::MissingRequiredScope(required.clone()));
            }
        }
        if (setup.webhook_configured && !definition.webhook_supported)
            || (setup.socket_or_polling_configured && !definition.socket_or_polling_supported)
        {
            return Err(ChannelCatalogError::IngressMismatch);
        }
        let key = (kind, setup.account_id.clone());
        let registration = ChannelAdapterRegistration {
            kind,
            account_id: setup.account_id.clone(),
            capabilities,
            setup,
        };
        match self.accounts.entry(key) {
            std::collections::btree_map::Entry::Vacant(entry) => Ok(entry.insert(registration)),
            std::collections::btree_map::Entry::Occupied(_) => {
                Err(ChannelCatalogError::DuplicateAccount)
            }
        }
    }

    pub fn account(
        &self,
        kind: ChannelAdapterKind,
        account_id: &str,
    ) -> Option<&ChannelAdapterRegistration> {
        self.accounts.get(&(kind, account_id.to_owned()))
    }

    pub fn accounts(&self) -> impl ExactSizeIterator<Item = &ChannelAdapterRegistration> {
        self.accounts.values()
    }

    pub fn remove_account(
        &mut self,
        kind: ChannelAdapterKind,
        account_id: &str,
    ) -> Option<ChannelAdapterRegistration> {
        self.accounts.remove(&(kind, account_id.to_owned()))
    }
}

macro_rules! managed_adapter {
    ($adapter:ty, $kind:expr, $setup:ident) => {
        impl ManagedChannelAdapter for $adapter {
            fn kind(&self) -> ChannelAdapterKind {
                $kind
            }

            fn account_setup(&self) -> ChannelAccountSetupV2 {
                self.$setup()
            }

            fn test_connection(&self) -> Result<(), ChannelAdapterErrorV2> {
                <$adapter>::test_connection(self)
            }
        }
    };
}

managed_adapter!(
    DiscordAdapter,
    ChannelAdapterKind::Discord,
    account_setup_v2
);
managed_adapter!(SlackAdapter, ChannelAdapterKind::Slack, setup_diagnostics);
managed_adapter!(
    TelegramAdapter,
    ChannelAdapterKind::Telegram,
    account_setup_v2
);
managed_adapter!(
    WhatsAppCloudAdapter,
    ChannelAdapterKind::WhatsAppCloud,
    account_setup_v2
);
managed_adapter!(
    TeamsAdapter,
    ChannelAdapterKind::MicrosoftTeams,
    account_setup_v2
);
managed_adapter!(
    GoogleChatAdapter,
    ChannelAdapterKind::GoogleChat,
    account_setup_v2
);
managed_adapter!(EmailAdapter, ChannelAdapterKind::Email, account_setup_v2);
managed_adapter!(MatrixAdapter, ChannelAdapterKind::Matrix, account_setup_v2);

fn built_in_definition(kind: ChannelAdapterKind) -> ChannelAdapterDefinition {
    let names = |values: &[&str]| values.iter().map(|value| (*value).to_owned()).collect();
    let (credentials, scopes, webhook_supported, socket_or_polling_supported) = match kind {
        ChannelAdapterKind::Discord => (names(&["bot_token"]), names(&["bot"]), false, true),
        ChannelAdapterKind::Slack => (
            names(&["bot_token", "signing_secret"]),
            names(&["app_mentions:read", "chat:write"]),
            true,
            true,
        ),
        ChannelAdapterKind::Telegram => (
            names(&["bot_token", "webhook_secret"]),
            BTreeSet::new(),
            true,
            true,
        ),
        ChannelAdapterKind::WhatsAppCloud => (
            names(&["access_token", "app_secret", "verify_token"]),
            names(&["whatsapp_business_messaging"]),
            true,
            false,
        ),
        ChannelAdapterKind::MicrosoftTeams => (
            names(&[
                "bot_framework_access_token",
                "bot_framework_request_verifier",
            ]),
            names(&["https://api.botframework.com/.default"]),
            true,
            false,
        ),
        ChannelAdapterKind::GoogleChat => (
            names(&["chat_api_access_token", "google_chat_request_verifier"]),
            names(&["https://www.googleapis.com/auth/chat.bot"]),
            true,
            false,
        ),
        ChannelAdapterKind::Email => (
            names(&["provider_access_token", "webhook_signature_verifier"]),
            BTreeSet::new(),
            true,
            true,
        ),
        ChannelAdapterKind::Matrix => (names(&["access_token"]), BTreeSet::new(), false, true),
    };
    ChannelAdapterDefinition {
        kind,
        channel: kind.channel().to_owned(),
        required_credential_names: credentials,
        required_scopes: scopes,
        webhook_supported,
        socket_or_polling_supported,
        safe_test_supported: true,
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "event", content = "payload")]
pub enum NormalizedPlatformEvent {
    Message(Box<InboundMessage>),
    RateLimited { retry_after_ms: u64 },
    Disconnected { safe_reason: String },
}

impl From<NormalizedPlatformEvent> for AdapterEvent {
    fn from(event: NormalizedPlatformEvent) -> Self {
        match event {
            NormalizedPlatformEvent::Message(message) => Self::Inbound(message),
            NormalizedPlatformEvent::RateLimited { retry_after_ms } => {
                Self::RateLimited { retry_after_ms }
            }
            NormalizedPlatformEvent::Disconnected { safe_reason } => {
                Self::Disconnected { safe_reason }
            }
        }
    }
}

pub struct JsonLineAdapter<S> {
    stream: BufReader<S>,
    features: AdapterFeatures,
    max_event_bytes: u64,
}

impl<S: Read + Write> JsonLineAdapter<S> {
    pub fn new(stream: S, features: AdapterFeatures, max_event_bytes: u64) -> Self {
        Self {
            stream: BufReader::new(stream),
            features,
            max_event_bytes,
        }
    }
}

impl<S: Read + Write> ChannelAdapter for JsonLineAdapter<S> {
    fn features(&self) -> AdapterFeatures {
        self.features.clone()
    }

    fn receive(&mut self) -> Result<AdapterEvent, AdapterFailure> {
        let mut bytes = Vec::new();
        let read = self
            .stream
            .by_ref()
            .take(self.max_event_bytes.saturating_add(1))
            .read_until(b'\n', &mut bytes)
            .map_err(|error| io_failure(&error))?;
        if read == 0 {
            return Ok(AdapterEvent::Disconnected {
                safe_reason: "platform stream closed".to_owned(),
            });
        }
        if u64::try_from(bytes.len()).unwrap_or(u64::MAX) > self.max_event_bytes {
            return Err(AdapterFailure {
                class: RetryClass::Permanent,
                safe_message: "platform event exceeds the configured limit".to_owned(),
                retry_after_ms: None,
            });
        }
        let event = serde_json::from_slice::<NormalizedPlatformEvent>(&bytes).map_err(|_| {
            AdapterFailure {
                class: RetryClass::Permanent,
                safe_message: "malformed platform event".to_owned(),
                retry_after_ms: None,
            }
        })?;
        Ok(event.into())
    }

    fn send(&mut self, message: &OutboundMessage) -> Result<SendReceipt, AdapterFailure> {
        let mut bytes = serde_json::to_vec(message).map_err(|_| AdapterFailure {
            class: RetryClass::Permanent,
            safe_message: "outbound message could not be encoded".to_owned(),
            retry_after_ms: None,
        })?;
        bytes.push(b'\n');
        self.stream
            .get_mut()
            .write_all(&bytes)
            .map_err(|error| io_failure(&error))?;
        self.stream
            .get_mut()
            .flush()
            .map_err(|error| io_failure(&error))?;
        Ok(SendReceipt {
            platform_message_id: message.idempotency_key.clone(),
            accepted_at: UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH),
            duplicate_possible: true,
        })
    }

    fn reconnect(&mut self) -> Result<(), AdapterFailure> {
        Err(AdapterFailure {
            class: RetryClass::Permanent,
            safe_message: "this stream adapter does not own its transport reconnect".to_owned(),
            retry_after_ms: None,
        })
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DiscordConfig {
    pub api_base: String,
    pub gateway_url: String,
    pub bot_user_id: String,
    pub intents: u64,
    pub max_event_bytes: usize,
    pub max_attachment_bytes: u64,
    pub max_staged_bytes: usize,
    pub timeout_ms: u64,
    pub deduplication_capacity: usize,
}

impl DiscordConfig {
    pub fn production(bot_user_id: impl Into<String>, intents: u64) -> Self {
        Self {
            api_base: "https://discord.com/api/v10".to_owned(),
            gateway_url: "wss://gateway.discord.gg/?v=10&encoding=json".to_owned(),
            bot_user_id: bot_user_id.into(),
            intents,
            max_event_bytes: 1024 * 1_024,
            max_attachment_bytes: 25 * 1_024 * 1_024,
            max_staged_bytes: 50 * 1_024 * 1_024,
            timeout_ms: 30_000,
            deduplication_capacity: 4_096,
        }
    }
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DiscordCursor {
    pub session_id: Option<String>,
    pub sequence: Option<u64>,
    pub resume_gateway_url: Option<String>,
    pub recent_message_ids: Vec<String>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DiscordUpload {
    pub file_name: String,
    pub media_type: String,
    pub bytes: Vec<u8>,
}

type DiscordSocket = WebSocket<MaybeTlsStream<TcpStream>>;

pub struct DiscordAdapter {
    config: DiscordConfig,
    token: SecretValue,
    http: ureq::Agent,
    socket: Option<DiscordSocket>,
    cursor: DiscordCursor,
    seen: BTreeSet<String>,
    seen_order: VecDeque<String>,
    last_conversation_kind: Option<(String, ChannelConversationKindV2)>,
    last_mentions: Vec<ChannelMentionV2>,
    staged: BTreeMap<ArtifactId, DiscordUpload>,
    staged_bytes: usize,
}

impl DiscordAdapter {
    /// Creates a disconnected Discord Bot adapter without exposing its token.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid endpoints or zero bounds.
    pub fn new(
        config: DiscordConfig,
        token: SecretValue,
        cursor: DiscordCursor,
    ) -> Result<Self, AdapterFailure> {
        if config.bot_user_id.trim().is_empty()
            || config.max_event_bytes == 0
            || config.max_attachment_bytes == 0
            || config.max_staged_bytes == 0
            || config.timeout_ms == 0
            || config.deduplication_capacity == 0
            || !(config.api_base.starts_with("https://")
                || config.api_base.starts_with("http://127.0.0.1:")
                || config.api_base.starts_with("http://localhost:"))
            || !(config.gateway_url.starts_with("wss://")
                || config.gateway_url.starts_with("ws://127.0.0.1:")
                || config.gateway_url.starts_with("ws://localhost:"))
        {
            return Err(permanent("invalid Discord adapter configuration"));
        }
        let http: ureq::Agent = ureq::Agent::config_builder()
            .timeout_global(Some(Duration::from_millis(config.timeout_ms)))
            .http_status_as_error(false)
            .build()
            .into();
        let seen_order = cursor
            .recent_message_ids
            .iter()
            .rev()
            .take(config.deduplication_capacity)
            .cloned()
            .collect::<Vec<_>>()
            .into_iter()
            .rev()
            .collect::<VecDeque<_>>();
        let seen = seen_order.iter().cloned().collect();
        Ok(Self {
            config,
            token,
            http,
            socket: None,
            cursor,
            seen,
            seen_order,
            last_conversation_kind: None,
            last_mentions: Vec::new(),
            staged: BTreeMap::new(),
            staged_bytes: 0,
        })
    }

    pub const fn cursor(&self) -> &DiscordCursor {
        &self.cursor
    }

    pub fn account_setup_v2(&self) -> ChannelAccountSetupV2 {
        ChannelAccountSetupV2 {
            account_id: self.config.bot_user_id.clone(),
            required_credential_names: BTreeSet::from(["bot_token".to_owned()]),
            required_scopes: BTreeSet::from(["bot".to_owned()]),
            webhook_configured: false,
            socket_or_polling_configured: true,
            connection_health: if self.socket.is_some() {
                ChannelConnectionHealthV2::Connected
            } else {
                ChannelConnectionHealthV2::Disconnected
            },
            reconnect_cursor_present: self.cursor.session_id.is_some()
                || self.cursor.sequence.is_some()
                || !self.cursor.recent_message_ids.is_empty(),
            safe_test_supported: true,
            metadata: BTreeMap::from([
                ("gateway".to_owned(), "discord_gateway_v10".to_owned()),
                ("intents".to_owned(), self.config.intents.to_string()),
            ]),
        }
    }

    /// Replaces the in-memory Discord credential. The old secret is zeroed on drop.
    pub fn rotate_token(&mut self, token: SecretValue) {
        self.token = token;
        self.socket = None;
    }

    /// Performs Discord's read-only current-user request and verifies account continuity.
    ///
    /// # Errors
    ///
    /// Returns a classified error for authentication, transport, malformed data, or an account
    /// mismatch.
    pub fn test_connection(&self) -> Result<(), ChannelAdapterErrorV2> {
        let url = format!("{}/users/@me", self.config.api_base);
        let mut response = self.token.with_bytes(|token| {
            let token = std::str::from_utf8(token).map_err(|_| ChannelAdapterErrorV2 {
                kind: ChannelAdapterErrorKindV2::Authentication,
                safe_message: "Discord token is not UTF-8".to_owned(),
                retry_after_ms: None,
            })?;
            self.http
                .get(&url)
                .header("Authorization", format!("Bot {token}"))
                .call()
                .map_err(|error| ChannelAdapterErrorV2::from(rest_transport_error(&error)))
        })?;
        let status = response.status().as_u16();
        let bytes = response
            .body_mut()
            .with_config()
            .limit(u64::try_from(self.config.max_event_bytes).unwrap_or(u64::MAX))
            .read_to_vec()
            .map_err(|error| ChannelAdapterErrorV2::from(rest_transport_error(&error)))?;
        match status {
            200..=299 => {
                let value: Value = serde_json::from_slice(&bytes).map_err(|_| {
                    ChannelAdapterErrorV2::malformed("Discord account test returned malformed JSON")
                })?;
                if value.get("id").and_then(Value::as_str) == Some(&self.config.bot_user_id) {
                    Ok(())
                } else {
                    Err(ChannelAdapterErrorV2 {
                        kind: ChannelAdapterErrorKindV2::Permission,
                        safe_message: "Discord credential belongs to another bot account"
                            .to_owned(),
                        retry_after_ms: None,
                    })
                }
            }
            401 | 403 => Err(ChannelAdapterErrorV2 {
                kind: ChannelAdapterErrorKindV2::Authentication,
                safe_message: "Discord authentication or permission denied".to_owned(),
                retry_after_ms: None,
            }),
            429 => Err(ChannelAdapterErrorV2 {
                kind: ChannelAdapterErrorKindV2::RateLimit,
                safe_message: "Discord account test was rate limited".to_owned(),
                retry_after_ms: None,
            }),
            500..=599 => Err(ChannelAdapterErrorV2 {
                kind: ChannelAdapterErrorKindV2::TransientNetwork,
                safe_message: "Discord account test is temporarily unavailable".to_owned(),
                retry_after_ms: None,
            }),
            _ => Err(ChannelAdapterErrorV2 {
                kind: ChannelAdapterErrorKindV2::PermanentDestination,
                safe_message: "Discord rejected the account test".to_owned(),
                retry_after_ms: None,
            }),
        }
    }

    /// Stages explicit artifact bytes for the next Discord multipart send.
    ///
    /// # Errors
    ///
    /// Returns an error when an individual or aggregate attachment bound is exceeded.
    pub fn stage_artifact(
        &mut self,
        id: ArtifactId,
        upload: DiscordUpload,
    ) -> Result<(), AdapterFailure> {
        if upload.file_name.trim().is_empty()
            || upload.media_type.trim().is_empty()
            || upload.bytes.is_empty()
            || u64::try_from(upload.bytes.len()).unwrap_or(u64::MAX)
                > self.config.max_attachment_bytes
        {
            return Err(permanent("Discord attachment is invalid or oversized"));
        }
        let old = self.staged.get(&id).map_or(0, |value| value.bytes.len());
        let next = self
            .staged_bytes
            .saturating_sub(old)
            .saturating_add(upload.bytes.len());
        if next > self.config.max_staged_bytes {
            return Err(permanent("Discord staged attachment budget exceeded"));
        }
        self.staged.insert(id, upload);
        self.staged_bytes = next;
        Ok(())
    }

    /// Emits Discord's supported typing indicator for a real route.
    ///
    /// # Errors
    ///
    /// Returns a classified REST, auth, or rate-limit failure.
    pub fn send_typing(
        &self,
        route: &keith_channel_core::ReplyRoute,
    ) -> Result<(), AdapterFailure> {
        self.rest_post(
            &format!("/channels/{}/typing", route.conversation),
            b"",
            "application/json",
            true,
        )?;
        Ok(())
    }

    fn connect_gateway(&mut self) -> Result<(), AdapterFailure> {
        let url = self
            .cursor
            .resume_gateway_url
            .as_deref()
            .unwrap_or(&self.config.gateway_url);
        let (mut socket, _) = connect(url).map_err(|error| gateway_error(&error))?;
        let hello = read_gateway_json(&mut socket, self.config.max_event_bytes)?;
        if hello.get("op").and_then(Value::as_u64) != Some(10) {
            return Err(permanent("Discord gateway did not send Hello"));
        }
        let authentication = if let (Some(session_id), Some(sequence)) =
            (&self.cursor.session_id, self.cursor.sequence)
        {
            self.token.with_bytes(|token| {
                let token = std::str::from_utf8(token)
                    .map_err(|_| permanent("Discord token is not UTF-8"))?;
                Ok(json!({
                    "op": 6,
                    "d": {"token": token, "session_id": session_id, "seq": sequence}
                }))
            })?
        } else {
            self.token.with_bytes(|token| {
                let token = std::str::from_utf8(token)
                    .map_err(|_| permanent("Discord token is not UTF-8"))?;
                Ok(json!({
                    "op": 2,
                    "d": {
                        "token": token,
                        "intents": self.config.intents,
                        "properties": {"os": std::env::consts::OS, "browser": "keith", "device": "keith"}
                    }
                }))
            })?
        };
        socket
            .send(Message::Text(authentication.to_string().into()))
            .map_err(|error| gateway_error(&error))?;
        self.socket = Some(socket);
        Ok(())
    }

    fn normalize_dispatch(
        &mut self,
        payload: &Value,
    ) -> Result<Option<AdapterEvent>, AdapterFailure> {
        if let Some(sequence) = payload.get("s").and_then(Value::as_u64) {
            self.cursor.sequence = Some(sequence);
        }
        let event_type = payload.get("t").and_then(Value::as_str).unwrap_or_default();
        let data = payload
            .get("d")
            .ok_or_else(|| permanent("malformed Discord dispatch"))?;
        if event_type == "READY" {
            self.cursor.session_id = data
                .get("session_id")
                .and_then(Value::as_str)
                .map(str::to_owned);
            self.cursor.resume_gateway_url = data
                .get("resume_gateway_url")
                .and_then(Value::as_str)
                .map(|url| format!("{url}/?v=10&encoding=json"));
            return Ok(None);
        }
        if event_type != "MESSAGE_CREATE" {
            return Ok(None);
        }
        let message_id = string_field(data, "id")?;
        if self.seen.contains(&message_id) {
            return Ok(None);
        }
        let author = data
            .get("author")
            .ok_or_else(|| permanent("Discord message has no author"))?;
        let sender = string_field(author, "id")?;
        if sender == self.config.bot_user_id {
            return Ok(None);
        }
        let attachments = discord_attachments(data, self.config.max_attachment_bytes)?;
        let occurred_at = discord_snowflake_timestamp(&message_id)
            .unwrap_or_else(|| UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH));
        self.last_conversation_kind = Some((
            message_id.clone(),
            if data.get("guild_id").and_then(Value::as_str).is_some() {
                ChannelConversationKindV2::Channel
            } else {
                ChannelConversationKindV2::Direct
            },
        ));
        self.last_mentions = discord_mentions(data);
        self.remember(message_id.clone());
        Ok(Some(AdapterEvent::Inbound(Box::new(InboundMessage {
            channel: "discord".to_owned(),
            external_account: self.config.bot_user_id.clone(),
            conversation: string_field(data, "channel_id")?,
            thread: data
                .get("thread_id")
                .and_then(Value::as_str)
                .map(str::to_owned),
            sender,
            message_id,
            reply_target: data
                .get("message_reference")
                .and_then(|reference| reference.get("message_id"))
                .and_then(Value::as_str)
                .map(str::to_owned),
            text: data
                .get("content")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_owned(),
            attachments,
            occurred_at,
            intent: InboundIntent::Prompt,
        }))))
    }

    fn remember(&mut self, message_id: String) {
        self.seen.insert(message_id.clone());
        self.seen_order.push_back(message_id);
        while self.seen_order.len() > self.config.deduplication_capacity {
            if let Some(expired) = self.seen_order.pop_front() {
                self.seen.remove(&expired);
            }
        }
        self.cursor.recent_message_ids = self.seen_order.iter().cloned().collect();
    }

    fn send_message(&mut self, message: &OutboundMessage) -> Result<SendReceipt, AdapterFailure> {
        if message.route.channel != "discord"
            || message.route.external_account != self.config.bot_user_id
        {
            return Err(permanent(
                "Discord delivery route does not belong to this adapter account",
            ));
        }
        if message.text.len() > 2_000 {
            return Err(permanent("Discord message exceeds 2000 characters"));
        }
        let mut payload = json!({
            "content": message.text,
            "nonce": message.idempotency_key,
            "enforce_nonce": true,
        });
        if let Some(reply) = &message.route.reply_to_message {
            payload["message_reference"] = json!({"message_id": reply});
        }
        let (body, content_type) = if message.artifacts.is_empty() {
            (
                serde_json::to_vec(&payload)
                    .map_err(|_| permanent("Discord payload encoding failed"))?,
                "application/json".to_owned(),
            )
        } else {
            build_multipart(&payload, &message.artifacts, &self.staged)?
        };
        let response = self.rest_post(
            &format!("/channels/{}/messages", message.route.conversation),
            &body,
            &content_type,
            false,
        )?;
        let platform_message_id = response
            .get("id")
            .and_then(Value::as_str)
            .ok_or_else(|| permanent("Discord send receipt has no message ID"))?
            .to_owned();
        for artifact in &message.artifacts {
            if let Some(upload) = self.staged.remove(artifact) {
                self.staged_bytes = self.staged_bytes.saturating_sub(upload.bytes.len());
            }
        }
        Ok(SendReceipt {
            platform_message_id,
            accepted_at: UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH),
            duplicate_possible: false,
        })
    }

    fn rest_post(
        &self,
        path: &str,
        body: &[u8],
        content_type: &str,
        empty_success: bool,
    ) -> Result<Value, AdapterFailure> {
        let url = format!("{}{path}", self.config.api_base);
        let mut response = self.token.with_bytes(|token| {
            let token =
                std::str::from_utf8(token).map_err(|_| permanent("Discord token is not UTF-8"))?;
            self.http
                .post(&url)
                .header("Authorization", format!("Bot {token}"))
                .header("Content-Type", content_type)
                .send(body)
                .map_err(|error| rest_transport_error(&error))
        })?;
        let status = response.status().as_u16();
        let response_body = response
            .body_mut()
            .with_config()
            .limit(u64::try_from(self.config.max_event_bytes).unwrap_or(u64::MAX))
            .read_to_vec()
            .map_err(|error| rest_transport_error(&error))?;
        match status {
            200..=299 if empty_success && response_body.is_empty() => Ok(Value::Null),
            200..=299 => serde_json::from_slice(&response_body)
                .map_err(|_| permanent("Discord returned malformed JSON")),
            401 | 403 => Err(permanent("Discord authentication or permission denied")),
            429 => {
                let retry_after_ms = serde_json::from_slice::<Value>(&response_body)
                    .ok()
                    .and_then(|body| body.get("retry_after").and_then(Value::as_f64))
                    .and_then(|seconds| Duration::try_from_secs_f64(seconds).ok())
                    .and_then(|duration| u64::try_from(duration.as_millis()).ok());
                Err(AdapterFailure {
                    class: RetryClass::RateLimited,
                    safe_message: "Discord rate limit reached".to_owned(),
                    retry_after_ms,
                })
            }
            500..=599 => Err(retryable("Discord service is temporarily unavailable")),
            _ => Err(permanent("Discord rejected the request")),
        }
    }
}

fn discord_attachments(
    data: &Value,
    max_attachment_bytes: u64,
) -> Result<Vec<Attachment>, AdapterFailure> {
    data.get("attachments")
        .and_then(Value::as_array)
        .map(|items| {
            items
                .iter()
                .map(|item| {
                    let byte_length = item
                        .get("size")
                        .and_then(Value::as_u64)
                        .ok_or_else(|| permanent("Discord attachment size is missing"))?;
                    if byte_length > max_attachment_bytes {
                        return Err(permanent("Discord inbound attachment is oversized"));
                    }
                    Ok(Attachment {
                        id: string_field(item, "id")?,
                        file_name: string_field(item, "filename")?,
                        media_type: item
                            .get("content_type")
                            .and_then(Value::as_str)
                            .unwrap_or("application/octet-stream")
                            .to_owned(),
                        byte_length,
                        artifact_id: None,
                        download_url: item.get("url").and_then(Value::as_str).map(str::to_owned),
                        staging_file: None,
                        sha256: None,
                    })
                })
                .collect::<Result<Vec<_>, _>>()
        })
        .transpose()
        .map(Option::unwrap_or_default)
}

fn discord_mentions(data: &Value) -> Vec<ChannelMentionV2> {
    data.get("mentions")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .filter_map(|mention| {
            Some(ChannelMentionV2 {
                identity: ChannelIdentityV2 {
                    platform_id: mention.get("id")?.as_str()?.to_owned(),
                    display_name: mention
                        .get("global_name")
                        .or_else(|| mention.get("username"))
                        .and_then(Value::as_str)
                        .map(str::to_owned),
                    is_bot: mention.get("bot").and_then(Value::as_bool).unwrap_or(false),
                },
                start: None,
                end: None,
            })
        })
        .collect()
}

impl ChannelAdapterV2 for DiscordAdapter {
    fn capabilities_v2(&self) -> ChannelCapabilitiesV2 {
        discord_capabilities(&self.config)
    }

    fn receive_v2(&mut self) -> Result<ChannelEventV2, ChannelAdapterErrorV2> {
        let event = ChannelAdapter::receive(self).map_err(ChannelAdapterErrorV2::from)?;
        let now = UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH);
        let event = match event {
            AdapterEvent::Inbound(message) => {
                let conversation_kind = self
                    .last_conversation_kind
                    .take()
                    .filter(|(message_id, _)| message_id == &message.message_id)
                    .map_or(ChannelConversationKindV2::Channel, |(_, kind)| kind);
                let attachments = message
                    .attachments
                    .iter()
                    .cloned()
                    .map(|attachment| ChannelAttachmentV2 {
                        kind: discord_attachment_kind(&attachment.media_type),
                        attachment,
                        duration_ms: None,
                        metadata: BTreeMap::new(),
                    })
                    .collect();
                let mentions = std::mem::take(&mut self.last_mentions);
                ChannelEventV2 {
                    contract: CHANNEL_CONTRACT_V2,
                    event_id: message.message_id.clone(),
                    delivery_attempt: 1,
                    event: ChannelEventKindV2::MessageCreated(ChannelMessageV2 {
                        message_id: message.message_id.clone(),
                        account_id: message.external_account.clone(),
                        conversation: ChannelConversationV2 {
                            platform_id: message.conversation.clone(),
                            kind: conversation_kind,
                            thread_id: message.thread.clone(),
                            reply_to_message_id: message.reply_target.clone(),
                        },
                        sender: ChannelIdentityV2 {
                            platform_id: message.sender.clone(),
                            display_name: None,
                            is_bot: false,
                        },
                        text: message.text.clone(),
                        attachments,
                        rich_content: Vec::new(),
                        mentions,
                        occurred_at: message.occurred_at,
                        metadata: BTreeMap::new(),
                    }),
                    metadata: BTreeMap::new(),
                }
            }
            AdapterEvent::RateLimited { retry_after_ms } => ChannelEventV2 {
                contract: CHANNEL_CONTRACT_V2,
                event_id: format!("discord-rate-limit:{}", now.unix_millis()),
                delivery_attempt: 1,
                event: ChannelEventKindV2::RateLimited { retry_after_ms },
                metadata: BTreeMap::new(),
            },
            AdapterEvent::Disconnected { safe_reason } => ChannelEventV2 {
                contract: CHANNEL_CONTRACT_V2,
                event_id: format!("discord-reconnect:{}", now.unix_millis()),
                delivery_attempt: 1,
                event: ChannelEventKindV2::ReconnectRequired { safe_reason },
                metadata: BTreeMap::new(),
            },
        };
        event.validate(&self.capabilities_v2())?;
        Ok(event)
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
                        "Discord rich embeds are not enabled by this adapter",
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
            ChannelOperationV2::SetTyping { route, active } if *active => {
                self.send_typing(route)
                    .map_err(ChannelAdapterErrorV2::from)?;
                Ok(ChannelOperationReceiptV2 {
                    operation_id: format!(
                        "discord-typing:{}:{}",
                        route.conversation,
                        UtcTimestamp::now()
                            .unwrap_or(UtcTimestamp::UNIX_EPOCH)
                            .unix_millis()
                    ),
                    platform_message_id: None,
                    accepted_at: UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH),
                    state: ChannelReceiptStateV2::Accepted,
                    duplicate_possible: false,
                    metadata: BTreeMap::new(),
                })
            }
            ChannelOperationV2::SetTyping { .. } => Err(ChannelAdapterErrorV2::unsupported(
                "Discord typing indicators expire automatically and cannot be explicitly stopped",
            )),
            ChannelOperationV2::EditMessage { .. }
            | ChannelOperationV2::DeleteMessage { .. }
            | ChannelOperationV2::AddReaction { .. }
            | ChannelOperationV2::RemoveReaction { .. }
            | ChannelOperationV2::Cancel { .. } => Err(ChannelAdapterErrorV2::unsupported(
                "Discord operation is not enabled by this adapter",
            )),
        }
    }

    fn reconnect_v2(&mut self) -> Result<(), ChannelAdapterErrorV2> {
        ChannelAdapter::reconnect(self).map_err(ChannelAdapterErrorV2::from)
    }

    fn reconnect_cursor_v2(&self) -> Option<ReconnectCursorV2> {
        if self.cursor.session_id.is_none()
            && self.cursor.sequence.is_none()
            && self.cursor.recent_message_ids.is_empty()
        {
            return None;
        }
        let observed_at = self
            .cursor
            .recent_message_ids
            .last()
            .and_then(|message_id| discord_snowflake_timestamp(message_id))
            .unwrap_or_else(|| UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH));
        serde_json::to_string(&self.cursor)
            .ok()
            .map(|value| ReconnectCursorV2 { value, observed_at })
    }
}

fn discord_capabilities(config: &DiscordConfig) -> ChannelCapabilitiesV2 {
    let mut declarations = ChannelCapabilityV2::ALL
        .into_iter()
        .map(|capability| {
            (
                capability,
                ChannelCapabilitySupportV2::Unsupported {
                    safe_reason: "Discord capability is not enabled by this adapter".to_owned(),
                },
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
        ChannelCapabilityV2::Typing,
        ChannelCapabilityV2::RateLimits,
        ChannelCapabilityV2::Reconnect,
        ChannelCapabilityV2::IdempotentSend,
    ] {
        declarations.insert(capability, ChannelCapabilitySupportV2::Supported);
    }
    ChannelCapabilitiesV2 {
        contract: CHANNEL_CONTRACT_V2,
        declarations,
        max_event_bytes: u64::try_from(config.max_event_bytes).unwrap_or(u64::MAX),
        max_attachment_bytes: config.max_attachment_bytes,
        max_attachments: 10,
        max_rich_content_bytes: 1,
        requests_per_minute: Some(50),
    }
}

fn discord_attachment_kind(media_type: &str) -> ChannelAttachmentKindV2 {
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

impl ChannelAdapter for DiscordAdapter {
    fn features(&self) -> AdapterFeatures {
        AdapterFeatures {
            capabilities: BTreeSet::from([
                AdapterCapability::Attachments,
                AdapterCapability::Threads,
                AdapterCapability::IdempotentSend,
                AdapterCapability::Reconnect,
            ]),
            max_attachment_bytes: self.config.max_attachment_bytes,
            requests_per_minute: Some(50),
        }
    }

    fn receive(&mut self) -> Result<AdapterEvent, AdapterFailure> {
        if self.socket.is_none() {
            self.connect_gateway()?;
        }
        loop {
            let payload = read_gateway_json(
                self.socket.as_mut().expect("gateway connected"),
                self.config.max_event_bytes,
            )?;
            match payload.get("op").and_then(Value::as_u64) {
                Some(0) => {
                    if let Some(event) = self.normalize_dispatch(&payload)? {
                        return Ok(event);
                    }
                }
                Some(1) => {
                    let heartbeat = json!({"op": 1, "d": self.cursor.sequence});
                    self.socket
                        .as_mut()
                        .expect("gateway connected")
                        .send(Message::Text(heartbeat.to_string().into()))
                        .map_err(|error| gateway_error(&error))?;
                }
                Some(7 | 9) => {
                    self.socket = None;
                    return Ok(AdapterEvent::Disconnected {
                        safe_reason: "Discord requested reconnect".to_owned(),
                    });
                }
                Some(11) => {}
                _ => return Err(permanent("Discord gateway opcode is unsupported")),
            }
        }
    }

    fn send(&mut self, message: &OutboundMessage) -> Result<SendReceipt, AdapterFailure> {
        self.send_message(message)
    }

    fn reconnect(&mut self) -> Result<(), AdapterFailure> {
        self.socket = None;
        self.connect_gateway()
    }
}

fn read_gateway_json(
    socket: &mut DiscordSocket,
    max_event_bytes: usize,
) -> Result<Value, AdapterFailure> {
    loop {
        let message = socket.read().map_err(|error| gateway_error(&error))?;
        let bytes = match message {
            Message::Text(text) => text.as_bytes().to_vec(),
            Message::Binary(bytes) => bytes.to_vec(),
            Message::Ping(bytes) => {
                socket
                    .send(Message::Pong(bytes))
                    .map_err(|error| gateway_error(&error))?;
                continue;
            }
            Message::Pong(_) | Message::Frame(_) => continue,
            Message::Close(_) => return Err(reconnect_failure("Discord gateway closed")),
        };
        if bytes.len() > max_event_bytes {
            return Err(permanent("Discord gateway event exceeded its bound"));
        }
        return serde_json::from_slice(&bytes)
            .map_err(|_| permanent("Discord gateway sent malformed JSON"));
    }
}

fn build_multipart(
    payload: &Value,
    artifacts: &[ArtifactId],
    staged: &BTreeMap<ArtifactId, DiscordUpload>,
) -> Result<(Vec<u8>, String), AdapterFailure> {
    let boundary = "keith-discord-boundary";
    let mut payload = payload.clone();
    payload["attachments"] = Value::Array(
        artifacts
            .iter()
            .enumerate()
            .map(|(index, id)| {
                let upload = staged
                    .get(id)
                    .ok_or_else(|| permanent("Discord artifact bytes were not staged"))?;
                Ok(json!({"id": index, "filename": upload.file_name}))
            })
            .collect::<Result<Vec<_>, AdapterFailure>>()?,
    );
    let mut body = Vec::new();
    write!(
        body,
        "--{boundary}\r\nContent-Disposition: form-data; name=\"payload_json\"\r\nContent-Type: application/json\r\n\r\n{payload}\r\n"
    )
    .map_err(|_| permanent("Discord multipart encoding failed"))?;
    for (index, id) in artifacts.iter().enumerate() {
        let upload = staged
            .get(id)
            .ok_or_else(|| permanent("Discord artifact bytes were not staged"))?;
        write!(
            body,
            "--{boundary}\r\nContent-Disposition: form-data; name=\"files[{index}]\"; filename=\"{}\"\r\nContent-Type: {}\r\n\r\n",
            safe_header_value(&upload.file_name)?,
            safe_header_value(&upload.media_type)?
        )
        .map_err(|_| permanent("Discord multipart encoding failed"))?;
        body.extend_from_slice(&upload.bytes);
        body.extend_from_slice(b"\r\n");
    }
    write!(body, "--{boundary}--\r\n")
        .map_err(|_| permanent("Discord multipart encoding failed"))?;
    Ok((body, format!("multipart/form-data; boundary={boundary}")))
}

fn safe_header_value(value: &str) -> Result<&str, AdapterFailure> {
    if value.contains(['\r', '\n', '"']) {
        Err(permanent(
            "Discord attachment metadata contains unsafe characters",
        ))
    } else {
        Ok(value)
    }
}

fn string_field(value: &Value, field: &str) -> Result<String, AdapterFailure> {
    value
        .get(field)
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty())
        .map(str::to_owned)
        .ok_or_else(|| permanent("Discord payload is missing an identity field"))
}

fn discord_snowflake_timestamp(value: &str) -> Option<UtcTimestamp> {
    value.parse::<u64>().ok().and_then(|snowflake| {
        let milliseconds = (snowflake >> 22).checked_add(1_420_070_400_000)?;
        i64::try_from(milliseconds)
            .ok()
            .map(UtcTimestamp::from_unix_millis)
    })
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

fn gateway_error(error: &tungstenite::Error) -> AdapterFailure {
    reconnect_failure(&format!("Discord gateway transport failed: {error}"))
}

fn rest_transport_error(error: &ureq::Error) -> AdapterFailure {
    match error {
        ureq::Error::Timeout(_) => retryable("Discord request timed out"),
        _ => reconnect_failure("Discord REST transport failed"),
    }
}

fn io_failure(error: &std::io::Error) -> AdapterFailure {
    AdapterFailure {
        class: RetryClass::Reconnect,
        safe_message: format!("platform transport failed: {}", error.kind()),
        retry_after_ms: None,
    }
}

#[cfg(test)]
mod tests {
    use std::io::BufReader;
    use std::net::TcpListener;
    use std::thread;

    use keith_agent_types::ArtifactId;
    use keith_channel_core::{
        AdapterCapability, GatewayLimits, GatewayQueue, InboundIntent, RoutedInbound,
    };
    use keith_connection::local_stream_pair;

    use super::*;

    fn discord_config(api_base: String, gateway_url: String) -> DiscordConfig {
        DiscordConfig {
            api_base,
            gateway_url,
            bot_user_id: "999".to_owned(),
            intents: 1 << 9,
            max_event_bytes: 64 * 1_024,
            max_attachment_bytes: 1_024,
            max_staged_bytes: 2_048,
            timeout_ms: 2_000,
            deduplication_capacity: 32,
        }
    }

    fn discord_token() -> SecretValue {
        SecretValue::new(b"test-token".to_vec()).expect("Discord token")
    }

    fn discord_message(id: &str, author: &str, content: &str, attachment: bool) -> Value {
        json!({
            "op": 0,
            "s": 3,
            "t": "MESSAGE_CREATE",
            "d": {
                "id": id,
                "channel_id": "456",
                "guild_id": "789",
                "author": {"id": author},
                "content": content,
                "attachments": if attachment {
                    json!([{"id":"55","filename":"report.txt","content_type":"text/plain","size":12}])
                } else {
                    json!([])
                }
            }
        })
    }

    fn http_request(stream: &mut std::net::TcpStream) -> (String, Vec<u8>) {
        let mut reader = BufReader::new(stream.try_clone().expect("clone HTTP stream"));
        let mut headers = String::new();
        let mut content_length = 0;
        loop {
            let mut line = String::new();
            reader.read_line(&mut line).expect("HTTP header");
            if line == "\r\n" {
                break;
            }
            if line.to_ascii_lowercase().starts_with("content-length:") {
                content_length = line
                    .split_once(':')
                    .expect("content length")
                    .1
                    .trim()
                    .parse::<usize>()
                    .expect("numeric content length");
            }
            headers.push_str(&line);
        }
        let mut body = vec![0; content_length];
        reader.read_exact(&mut body).expect("HTTP body");
        (headers, body)
    }

    fn serve_discord_gateway(listener: &TcpListener, address: std::net::SocketAddr) {
        let (stream, _) = listener.accept().expect("first gateway connection");
        let mut socket = tungstenite::accept(stream).expect("WebSocket handshake");
        socket
            .send(Message::Text(
                json!({"op":10,"d":{"heartbeat_interval":45_000}})
                    .to_string()
                    .into(),
            ))
            .expect("Hello");
        let identify: Value =
            serde_json::from_str(socket.read().expect("Identify").to_text().expect("text"))
                .expect("Identify JSON");
        assert_eq!(identify["op"], 2);
        socket
            .send(Message::Text(
                json!({
                    "op":0,"s":1,"t":"READY",
                    "d":{"session_id":"session","resume_gateway_url":format!("ws://{address}")}
                })
                .to_string()
                .into(),
            ))
            .expect("Ready");
        socket
            .send(Message::Text(
                discord_message("175928847299117062", "999", "own", false)
                    .to_string()
                    .into(),
            ))
            .expect("own message");
        let message = discord_message("175928847299117063", "123", "hello", true);
        socket
            .send(Message::Text(message.to_string().into()))
            .expect("message");
        socket
            .send(Message::Text(message.to_string().into()))
            .expect("duplicate");
        socket
            .send(Message::Text(json!({"op":7,"d":null}).to_string().into()))
            .expect("reconnect request");
        drop(socket);

        let (stream, _) = listener.accept().expect("resume gateway connection");
        let mut socket = tungstenite::accept(stream).expect("resume WebSocket handshake");
        socket
            .send(Message::Text(
                json!({"op":10,"d":{"heartbeat_interval":45_000}})
                    .to_string()
                    .into(),
            ))
            .expect("resume Hello");
        let resume: Value =
            serde_json::from_str(socket.read().expect("Resume").to_text().expect("text"))
                .expect("Resume JSON");
        assert_eq!(resume["op"], 6);
        assert_eq!(resume["d"]["session_id"], "session");
        socket
            .send(Message::Text(
                discord_message("175928847299117063", "123", "replayed", true)
                    .to_string()
                    .into(),
            ))
            .expect("replayed message");
        socket
            .send(Message::Text(
                discord_message("175928847299117064", "124", "after restart", false)
                    .to_string()
                    .into(),
            ))
            .expect("post-resume message");
    }

    fn features() -> AdapterFeatures {
        AdapterFeatures {
            capabilities: std::collections::BTreeSet::from([
                AdapterCapability::Attachments,
                AdapterCapability::Threads,
                AdapterCapability::Steering,
                AdapterCapability::Cancellation,
            ]),
            max_attachment_bytes: 4,
            requests_per_minute: Some(60),
        }
    }

    fn platform_message(message_id: &str, occurred_at: i64) -> NormalizedPlatformEvent {
        NormalizedPlatformEvent::Message(Box::new(InboundMessage {
            channel: "json".to_owned(),
            external_account: "account".to_owned(),
            conversation: "conversation".to_owned(),
            thread: Some("thread".to_owned()),
            sender: "sender".to_owned(),
            message_id: message_id.to_owned(),
            reply_target: None,
            text: "hello".to_owned(),
            attachments: Vec::new(),
            occurred_at: UtcTimestamp::from_unix_millis(occurred_at),
            intent: InboundIntent::Prompt,
        }))
    }

    #[test]
    fn conformance_covers_malformed_reordered_duplicate_oversized_rate_limit_and_disconnect() {
        let (platform, gateway) = local_stream_pair().expect("real local platform stream");
        let platform_thread = thread::spawn(move || {
            let mut platform = platform;
            for event in [
                platform_message("first", 20),
                platform_message("second", 10),
                platform_message("first", 20),
                NormalizedPlatformEvent::RateLimited {
                    retry_after_ms: 250,
                },
                NormalizedPlatformEvent::Disconnected {
                    safe_reason: "reconnect requested".to_owned(),
                },
            ] {
                serde_json::to_writer(&mut platform, &event).expect("encode event");
                platform.write_all(b"\n").expect("event delimiter");
            }
            platform.write_all(b"not-json\n").expect("malformed event");
        });
        let mut adapter = JsonLineAdapter::new(gateway, features(), 1_024);
        let session_id = keith_agent_types::SessionId::new();
        let profile_id = keith_agent_types::ProfileId::new();
        let mut queue = GatewayQueue::new(GatewayLimits {
            max_attachment_bytes: 4,
            max_total_attachment_bytes: 4,
            ..GatewayLimits::default()
        })
        .expect("queue");
        for expected in ["first", "second"] {
            let AdapterEvent::Inbound(message) = adapter.receive().expect("inbound") else {
                panic!("message required");
            };
            assert_eq!(message.message_id, expected);
            queue
                .enqueue(RoutedInbound {
                    profile_id: profile_id.clone(),
                    session_id: session_id.clone(),
                    message: *message,
                })
                .expect("queue inbound");
        }
        let AdapterEvent::Inbound(duplicate) = adapter.receive().expect("duplicate inbound") else {
            panic!("message required");
        };
        assert_eq!(
            queue
                .enqueue(RoutedInbound {
                    profile_id,
                    session_id,
                    message: *duplicate,
                })
                .expect("deduplicate"),
            keith_channel_core::EnqueueOutcome::Duplicate
        );
        assert_eq!(
            adapter.receive().expect("rate limit"),
            AdapterEvent::RateLimited {
                retry_after_ms: 250
            }
        );
        assert!(matches!(
            adapter.receive().expect("disconnect"),
            AdapterEvent::Disconnected { .. }
        ));
        assert_eq!(
            adapter.receive().expect_err("malformed denied").class,
            RetryClass::Permanent
        );
        platform_thread.join().expect("platform completes");
    }

    #[test]
    fn conformance_bounds_input_sends_receipts_and_classifies_reconnect() {
        let (mut oversized_platform, oversized_gateway) =
            local_stream_pair().expect("oversized stream");
        let oversized_thread = thread::spawn(move || {
            oversized_platform
                .write_all(&[b'x'; 33])
                .expect("oversized bytes");
        });
        let mut bounded = JsonLineAdapter::new(oversized_gateway, features(), 32);
        assert_eq!(
            bounded.receive().expect_err("oversized denied").class,
            RetryClass::Permanent
        );
        oversized_thread.join().expect("oversized writer");

        let (platform, gateway) = local_stream_pair().expect("outbound stream");
        let platform_thread = thread::spawn(move || {
            let mut platform = BufReader::new(platform);
            let mut outbound = String::new();
            platform
                .read_line(&mut outbound)
                .expect("read outbound message");
            serde_json::from_str::<OutboundMessage>(&outbound).expect("valid outbound message")
        });
        let mut adapter = JsonLineAdapter::new(gateway, features(), 1_024);
        let outbound = OutboundMessage {
            route: keith_channel_core::ReplyRoute {
                channel: "json".to_owned(),
                external_account: "account".to_owned(),
                conversation: "conversation".to_owned(),
                thread: None,
                reply_to_message: None,
            },
            idempotency_key: "delivery".to_owned(),
            text: "reply".to_owned(),
            artifacts: vec![ArtifactId::new()],
        };
        assert!(adapter.send(&outbound).is_ok());
        assert_eq!(
            platform_thread.join().expect("platform completes"),
            outbound
        );
        assert_eq!(
            adapter
                .reconnect()
                .expect_err("reconnect unsupported")
                .class,
            RetryClass::Permanent
        );
    }

    #[test]
    fn discord_gateway_inbound_dedup_attachment_isolation_and_resume_are_real() {
        let listener = TcpListener::bind("127.0.0.1:0").expect("gateway listener");
        let address = listener.local_addr().expect("gateway address");
        let server = thread::spawn(move || serve_discord_gateway(&listener, address));
        let unused_http = "http://127.0.0.1:9".to_owned();
        let config = discord_config(unused_http, format!("ws://{address}"));
        let mut adapter =
            DiscordAdapter::new(config.clone(), discord_token(), DiscordCursor::default())
                .expect("Discord adapter");
        let AdapterEvent::Inbound(message) = adapter.receive().expect("inbound") else {
            panic!("Discord inbound required");
        };
        assert_eq!(message.sender, "123");
        assert_eq!(message.attachments.len(), 1);
        assert_eq!(message.external_account, "999");
        assert!(matches!(
            adapter.receive().expect("duplicate skipped"),
            AdapterEvent::Disconnected { .. }
        ));
        assert_eq!(adapter.cursor().session_id.as_deref(), Some("session"));
        assert_eq!(adapter.cursor().recent_message_ids.len(), 1);
        let cursor = adapter.cursor().clone();
        drop(adapter);
        let mut adapter =
            DiscordAdapter::new(config, discord_token(), cursor).expect("restart Discord adapter");
        adapter.reconnect().expect("resume connection");
        let AdapterEvent::Inbound(restarted) = adapter.receive().expect("post-restart inbound")
        else {
            panic!("post-restart inbound required");
        };
        assert_eq!(restarted.text, "after restart");
        server.join().expect("gateway server");
    }

    #[test]
    fn discord_rest_reply_schedule_typing_attachment_receipt_rate_limit_and_failure_are_real() {
        let listener = TcpListener::bind("127.0.0.1:0").expect("REST listener");
        let address = listener.local_addr().expect("REST address");
        let server = thread::spawn(move || {
            let mut requests = Vec::new();
            for index in 0..5 {
                let (mut stream, _) = listener.accept().expect("REST connection");
                let request = http_request(&mut stream);
                assert!(
                    request
                        .0
                        .to_ascii_lowercase()
                        .contains("authorization: bot test-token")
                );
                let (status, body) = match index {
                    0 => ("204 No Content", ""),
                    1 => ("200 OK", r#"{"id":"receipt-1"}"#),
                    2 => ("200 OK", r#"{"id":"receipt-2"}"#),
                    3 => ("429 Too Many Requests", r#"{"retry_after":0.25}"#),
                    _ => ("401 Unauthorized", "{}"),
                };
                write!(
                    stream,
                    "HTTP/1.1 {status}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                    body.len()
                )
                .expect("REST response");
                requests.push(request);
            }
            requests
        });
        let mut adapter = DiscordAdapter::new(
            discord_config(format!("http://{address}"), "ws://127.0.0.1:9".to_owned()),
            discord_token(),
            DiscordCursor::default(),
        )
        .expect("Discord adapter");
        let route = keith_channel_core::ReplyRoute {
            channel: "discord".to_owned(),
            external_account: "999".to_owned(),
            conversation: "456".to_owned(),
            thread: None,
            reply_to_message: Some("original".to_owned()),
        };
        adapter.send_typing(&route).expect("typing");
        let scheduled = OutboundMessage {
            route: route.clone(),
            idempotency_key: "scheduled:job:attempt-1".to_owned(),
            text: "scheduled result".to_owned(),
            artifacts: Vec::new(),
        };
        let receipt = adapter.send(&scheduled).expect("scheduled reply");
        assert_eq!(receipt.platform_message_id, "receipt-1");
        assert!(!receipt.duplicate_possible);
        let artifact = ArtifactId::new();
        adapter
            .stage_artifact(
                artifact.clone(),
                DiscordUpload {
                    file_name: "report.txt".to_owned(),
                    media_type: "text/plain".to_owned(),
                    bytes: b"report bytes".to_vec(),
                },
            )
            .expect("stage attachment");
        let attachment = OutboundMessage {
            route,
            idempotency_key: "reply:attachment".to_owned(),
            text: "attached".to_owned(),
            artifacts: vec![artifact],
        };
        assert_eq!(
            adapter
                .send(&attachment)
                .expect("attachment reply")
                .platform_message_id,
            "receipt-2"
        );
        let failure = adapter.send(&scheduled).expect_err("rate limited");
        assert_eq!(failure.class, RetryClass::RateLimited);
        assert_eq!(failure.retry_after_ms, Some(250));
        assert_eq!(
            adapter
                .send(&scheduled)
                .expect_err("authentication failure")
                .class,
            RetryClass::Permanent
        );
        let requests = server.join().expect("REST server");
        assert!(String::from_utf8_lossy(&requests[1].1).contains("scheduled:job:attempt-1"));
        assert!(String::from_utf8_lossy(&requests[2].1).contains("report bytes"));
    }
}
