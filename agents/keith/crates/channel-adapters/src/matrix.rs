#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet, VecDeque};
use std::time::Duration;

use keith_agent_types::{ArtifactId, ProfileId, UtcTimestamp};
use keith_channel_core::{
    AdapterCapability, AdapterEvent, AdapterFailure, AdapterFeatures, Attachment,
    ChannelAccountSetupV2, ChannelAdapter, ChannelAdapterErrorKindV2, ChannelAdapterErrorV2,
    ChannelAdapterV2, ChannelAttachmentKindV2, ChannelAttachmentV2, ChannelCapabilitiesV2,
    ChannelCapabilitySupportV2, ChannelCapabilityV2, ChannelConnectionHealthV2,
    ChannelContractVersion, ChannelConversationKindV2, ChannelConversationV2, ChannelEventKindV2,
    ChannelEventV2, ChannelIdentityV2, ChannelMessageDeleteV2, ChannelMessageEditV2,
    ChannelMessageV2, ChannelOperationReceiptV2, ChannelOperationV2, ChannelOutboundMessageV2,
    ChannelReactionActionV2, ChannelReactionV2, ChannelReceiptStateV2, InboundIntent,
    InboundMessage, OutboundMessage, ReconnectCursorV2, SendReceipt,
};
use keith_credentials::{CredentialRef, SecretValue};
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value, json};

use crate::teams::{
    PreparedHttpRequest, adapter_failure_from_v2, allowed_endpoint, bounded_recent,
    classify_test_status, credential_belongs_to, event_v2_to_v1, now, operation_receipt,
    percent_encode, permanent, rate_limited, retryable, transport_error, v2_error,
    valid_channel_identity,
};

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct MatrixConfig {
    pub homeserver: String,
    pub external_account: String,
    pub user_id: String,
    pub profile_id: ProfileId,
    pub credential_ref: CredentialRef,
    pub max_event_bytes: usize,
    pub max_attachment_bytes: u64,
    pub max_staged_bytes: usize,
    pub request_timeout_ms: u64,
    pub sync_timeout_ms: u64,
    pub deduplication_capacity: usize,
}

impl MatrixConfig {
    pub fn production(
        homeserver: impl Into<String>,
        external_account: impl Into<String>,
        user_id: impl Into<String>,
        profile_id: ProfileId,
        credential_ref: CredentialRef,
    ) -> Self {
        Self {
            homeserver: homeserver.into(),
            external_account: external_account.into(),
            user_id: user_id.into(),
            profile_id,
            credential_ref,
            max_event_bytes: 10 * 1024 * 1024,
            max_attachment_bytes: 100 * 1024 * 1024,
            max_staged_bytes: 200 * 1024 * 1024,
            request_timeout_ms: 45_000,
            sync_timeout_ms: 30_000,
            deduplication_capacity: 8192,
        }
    }
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct MatrixCursor {
    pub next_batch: Option<String>,
    pub recent_event_ids: Vec<String>,
    pub encrypted_rooms: BTreeSet<String>,
    #[serde(default)]
    pub last_sync_at: Option<UtcTimestamp>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
#[allow(clippy::struct_excessive_bools)]
pub struct MatrixCapabilities {
    pub sync: bool,
    pub rooms: bool,
    pub threads: bool,
    pub replies: bool,
    pub edits: bool,
    pub reactions: bool,
    pub redactions: bool,
    pub media: bool,
    pub typing: bool,
    pub receipts: bool,
    pub end_to_end_encryption: bool,
    pub reconnect: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct MatrixSetupDiagnostics {
    pub profile_id: ProfileId,
    pub external_account: String,
    pub user_id: String,
    pub credential_ref: CredentialRef,
    pub has_sync_cursor: bool,
    pub encrypted_room_count: usize,
    pub revoked: bool,
    pub capabilities: MatrixCapabilities,
    pub safe_error: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct MatrixUpload {
    pub file_name: String,
    pub media_type: String,
    pub bytes: Vec<u8>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum MatrixNormalizedEvent {
    Message(Box<InboundMessage>),
    Edit {
        room_id: String,
        sender: String,
        event_id: String,
        target_event_id: String,
        text: String,
        occurred_at: UtcTimestamp,
    },
    Reaction {
        room_id: String,
        sender: String,
        event_id: String,
        target_event_id: String,
        key: String,
        occurred_at: UtcTimestamp,
    },
    Redaction {
        room_id: String,
        sender: String,
        event_id: String,
        target_event_id: String,
        occurred_at: UtcTimestamp,
    },
    UnsupportedEncrypted {
        room_id: String,
        event_id: String,
    },
}

pub struct MatrixAdapter {
    config: MatrixConfig,
    access_token: SecretValue,
    http: ureq::Agent,
    cursor: MatrixCursor,
    seen: BTreeSet<String>,
    seen_order: VecDeque<String>,
    pending: VecDeque<MatrixNormalizedEvent>,
    staged: std::collections::BTreeMap<ArtifactId, MatrixUpload>,
    staged_bytes: usize,
    cancelled: BTreeSet<String>,
    revoked: bool,
    safe_error: Option<String>,
}

impl MatrixAdapter {
    /// # Errors
    ///
    /// Returns a permanent error for invalid homeserver, user, bounds, or credential scope.
    pub fn new(
        config: MatrixConfig,
        access_token: SecretValue,
        cursor: MatrixCursor,
    ) -> Result<Self, AdapterFailure> {
        if !allowed_endpoint(&config.homeserver)
            || !valid_channel_identity(&config.external_account)
            || !valid_matrix_user_id(&config.user_id)
            || config.max_event_bytes == 0
            || config.max_attachment_bytes == 0
            || config.max_staged_bytes == 0
            || config.request_timeout_ms == 0
            || config.sync_timeout_ms == 0
            || config.deduplication_capacity == 0
            || !credential_belongs_to(&config.credential_ref, &config.external_account)
        {
            return Err(permanent("invalid Matrix adapter configuration"));
        }
        let http: ureq::Agent = ureq::Agent::config_builder()
            .timeout_global(Some(Duration::from_millis(config.request_timeout_ms)))
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
            pending: VecDeque::new(),
            staged: std::collections::BTreeMap::new(),
            staged_bytes: 0,
            cancelled: BTreeSet::new(),
            revoked: false,
            safe_error: None,
        })
    }

    pub const fn cursor(&self) -> &MatrixCursor {
        &self.cursor
    }

    #[allow(clippy::unused_self)]
    pub fn capabilities(&self) -> MatrixCapabilities {
        MatrixCapabilities {
            sync: true,
            rooms: true,
            threads: true,
            replies: true,
            edits: true,
            reactions: true,
            redactions: true,
            media: true,
            typing: true,
            receipts: true,
            end_to_end_encryption: false,
            reconnect: true,
        }
    }

    pub fn setup_diagnostics(&self) -> MatrixSetupDiagnostics {
        MatrixSetupDiagnostics {
            profile_id: self.config.profile_id.clone(),
            external_account: self.config.external_account.clone(),
            user_id: self.config.user_id.clone(),
            credential_ref: self.config.credential_ref.clone(),
            has_sync_cursor: self.cursor.next_batch.is_some(),
            encrypted_room_count: self.cursor.encrypted_rooms.len(),
            revoked: self.revoked,
            capabilities: self.capabilities(),
            safe_error: self.safe_error.clone(),
        }
    }

    pub fn account_setup_v2(&self) -> ChannelAccountSetupV2 {
        ChannelAccountSetupV2 {
            account_id: self.config.external_account.clone(),
            required_credential_names: BTreeSet::from(["access_token".to_owned()]),
            required_scopes: BTreeSet::new(),
            webhook_configured: false,
            socket_or_polling_configured: true,
            connection_health: if self.revoked {
                ChannelConnectionHealthV2::Revoked
            } else if self.safe_error.is_some() {
                ChannelConnectionHealthV2::Failed
            } else if self.cursor.last_sync_at.is_some() {
                ChannelConnectionHealthV2::Connected
            } else {
                ChannelConnectionHealthV2::Disconnected
            },
            reconnect_cursor_present: self.cursor.next_batch.is_some(),
            safe_test_supported: true,
            metadata: BTreeMap::from([
                ("homeserver".to_owned(), self.config.homeserver.clone()),
                ("user_id".to_owned(), self.config.user_id.clone()),
                ("scope_model".to_owned(), "access-token".to_owned()),
            ]),
        }
    }

    /// Prepares the read-only Matrix whoami request used to test this account.
    ///
    /// # Errors
    ///
    /// Returns a permanent failure for a revoked account.
    pub fn prepare_test_connection(&self) -> Result<PreparedHttpRequest, AdapterFailure> {
        self.ensure_active()?;
        Ok(PreparedHttpRequest {
            method: "GET".to_owned(),
            url: format!(
                "{}/_matrix/client/v3/account/whoami",
                self.config.homeserver.trim_end_matches('/')
            ),
            content_type: "application/json".to_owned(),
            body: Vec::new(),
            idempotency_key: None,
        })
    }

    /// Runs Matrix whoami and verifies the access token belongs to the configured user.
    ///
    /// # Errors
    ///
    /// Returns a classified authentication, permission, rate-limit, transport, malformed, or
    /// account-isolation failure.
    pub fn test_connection(&self) -> Result<(), ChannelAdapterErrorV2> {
        let request = self
            .prepare_test_connection()
            .map_err(ChannelAdapterErrorV2::from)?;
        let mut response = self.access_token.with_bytes(|token| {
            let token = std::str::from_utf8(token).map_err(|_| {
                v2_error(
                    ChannelAdapterErrorKindV2::Authentication,
                    "Matrix access token is not UTF-8",
                )
            })?;
            self.http
                .get(&request.url)
                .header("Authorization", format!("Bearer {token}"))
                .call()
                .map_err(|error| ChannelAdapterErrorV2::from(transport_error("Matrix", &error)))
        })?;
        classify_test_status("Matrix", response.status().as_u16())?;
        let body = response
            .body_mut()
            .with_config()
            .limit(u64::try_from(self.config.max_event_bytes).unwrap_or(u64::MAX))
            .read_to_vec()
            .map_err(|error| ChannelAdapterErrorV2::from(transport_error("Matrix", &error)))?;
        let identity: MatrixWhoAmI = serde_json::from_slice(&body)
            .map_err(|_| ChannelAdapterErrorV2::malformed("Matrix whoami response is malformed"))?;
        if identity.user_id != self.config.user_id {
            return Err(v2_error(
                ChannelAdapterErrorKindV2::Permission,
                "Matrix credential resolved to a different user identity",
            ));
        }
        Ok(())
    }

    /// Stages bytes for a real Matrix media upload.
    ///
    /// # Errors
    ///
    /// Returns a permanent failure when media metadata or byte ceilings are invalid.
    pub fn stage_artifact(
        &mut self,
        id: ArtifactId,
        upload: MatrixUpload,
    ) -> Result<(), AdapterFailure> {
        self.ensure_active()?;
        if upload.file_name.trim().is_empty()
            || upload.media_type.trim().is_empty()
            || upload.bytes.is_empty()
            || u64::try_from(upload.bytes.len()).unwrap_or(u64::MAX)
                > self.config.max_attachment_bytes
        {
            return Err(permanent("Matrix media is invalid or oversized"));
        }
        let previous = self.staged.get(&id).map_or(0, |item| item.bytes.len());
        let next = self
            .staged_bytes
            .saturating_sub(previous)
            .saturating_add(upload.bytes.len());
        if next > self.config.max_staged_bytes {
            return Err(permanent("Matrix staged media budget exceeded"));
        }
        self.staged.insert(id, upload);
        self.staged_bytes = next;
        Ok(())
    }

    /// Executes one incremental `/sync`, durably advancing `next_batch` after decoding the
    /// complete bounded response.
    ///
    /// # Errors
    ///
    /// Returns classified transport, authentication, rate-limit, or malformed-response failures.
    pub fn sync_once(&mut self) -> Result<usize, AdapterFailure> {
        self.ensure_active()?;
        let url = format!(
            "{}/_matrix/client/v3/sync",
            self.config.homeserver.trim_end_matches('/')
        );
        let timeout = self.config.sync_timeout_ms.to_string();
        let response = self.access_token.with_bytes(|token| {
            let token = std::str::from_utf8(token)
                .map_err(|_| permanent("Matrix access token is not UTF-8"))?;
            let mut request = self
                .http
                .get(&url)
                .query("timeout", &timeout)
                .header("Authorization", format!("Bearer {token}"));
            if let Some(since) = &self.cursor.next_batch {
                request = request.query("since", since);
            }
            request
                .call()
                .map_err(|error| transport_error("Matrix", &error))
        })?;
        let response_value = self.read_json_response(response, "Matrix sync")?;
        let sync: MatrixSyncResponse = serde_json::from_value(response_value)
            .map_err(|_| permanent("Matrix returned a malformed sync response"))?;
        self.apply_sync(sync)
    }

    /// Applies a bounded real Matrix `/sync` response body through the same cursor and event
    /// normalization path used by [`Self::sync_once`].
    ///
    /// # Errors
    ///
    /// Returns a permanent failure for oversized or malformed response data.
    pub fn ingest_sync_response(&mut self, body: &[u8]) -> Result<usize, AdapterFailure> {
        self.ensure_active()?;
        if body.len() > self.config.max_event_bytes {
            return Err(permanent(
                "Matrix sync response exceeds the configured limit",
            ));
        }
        let sync: MatrixSyncResponse = serde_json::from_slice(body)
            .map_err(|_| permanent("Matrix returned a malformed sync response"))?;
        self.apply_sync(sync)
    }

    fn apply_sync(&mut self, sync: MatrixSyncResponse) -> Result<usize, AdapterFailure> {
        if sync.next_batch.trim().is_empty() {
            return Err(permanent("Matrix sync response has no next_batch cursor"));
        }
        let cursor_before = self.cursor.clone();
        let seen_before = self.seen.clone();
        let seen_order_before = self.seen_order.clone();
        let normalized = (|| {
            let mut normalized = VecDeque::new();
            for (room_id, room) in sync.rooms.join {
                if !valid_matrix_room_id(&room_id) {
                    return Err(permanent("Matrix sync response has a malformed room ID"));
                }
                if room
                    .state
                    .events
                    .iter()
                    .any(|event| event.event_type == "m.room.encryption")
                {
                    self.cursor.encrypted_rooms.insert(room_id.clone());
                }
                for event in room.timeline.events {
                    if let Some(event) = self.normalize_event(&room_id, event)? {
                        normalized.push_back(event);
                    }
                }
            }
            Ok::<_, AdapterFailure>(normalized)
        })();
        let mut normalized = match normalized {
            Ok(normalized) => normalized,
            Err(error) => {
                self.cursor = cursor_before;
                self.seen = seen_before;
                self.seen_order = seen_order_before;
                return Err(error);
            }
        };
        let count = normalized.len();
        self.pending.append(&mut normalized);
        self.cursor.next_batch = Some(sync.next_batch);
        self.cursor.last_sync_at = Some(now());
        self.safe_error = None;
        Ok(count)
    }

    /// Returns the next rich Matrix event, including edits, reactions, redactions, and explicit
    /// encrypted-event refusal that the current common channel contract cannot represent.
    ///
    /// # Errors
    ///
    /// Returns a classified failure from synchronization or account revocation.
    pub fn receive_rich(&mut self) -> Result<MatrixNormalizedEvent, AdapterFailure> {
        self.ensure_active()?;
        if self.pending.is_empty() {
            self.sync_once()?;
        }
        self.pending
            .pop_front()
            .ok_or_else(|| retryable("Matrix sync contained no supported timeline event"))
    }

    /// Sends an `m.replace` edit with an idempotent Matrix transaction ID.
    ///
    /// # Errors
    ///
    /// Returns classified validation, transport, authentication, or rate-limit failures.
    pub fn send_edit(
        &mut self,
        room_id: &str,
        target_event_id: &str,
        text: &str,
        transaction_id: &str,
    ) -> Result<SendReceipt, AdapterFailure> {
        if text.trim().is_empty() || !valid_matrix_event_id(target_event_id) {
            return Err(permanent("Matrix edit target or body is invalid"));
        }
        self.send_room_event(
            room_id,
            "m.room.message",
            transaction_id,
            &json!({
                "msgtype": "m.text",
                "body": format!("* {text}"),
                "m.new_content": {"msgtype": "m.text", "body": text},
                "m.relates_to": {"rel_type": "m.replace", "event_id": target_event_id},
            }),
        )
    }

    /// Sends an `m.annotation` reaction with an idempotent transaction ID.
    ///
    /// # Errors
    ///
    /// Returns classified validation, transport, authentication, or rate-limit failures.
    pub fn send_reaction(
        &mut self,
        room_id: &str,
        target_event_id: &str,
        key: &str,
        transaction_id: &str,
    ) -> Result<SendReceipt, AdapterFailure> {
        if key.trim().is_empty() || !valid_matrix_event_id(target_event_id) {
            return Err(permanent("Matrix reaction target or key is invalid"));
        }
        self.send_room_event(
            room_id,
            "m.reaction",
            transaction_id,
            &json!({
                "m.relates_to": {
                    "rel_type": "m.annotation",
                    "event_id": target_event_id,
                    "key": key,
                }
            }),
        )
    }

    /// Sends an idempotent Matrix redaction event.
    ///
    /// # Errors
    ///
    /// Returns classified validation, transport, authentication, or rate-limit failures.
    pub fn send_redaction(
        &mut self,
        room_id: &str,
        target_event_id: &str,
        transaction_id: &str,
    ) -> Result<SendReceipt, AdapterFailure> {
        self.ensure_active()?;
        if !valid_matrix_room_id(room_id)
            || !valid_matrix_event_id(target_event_id)
            || transaction_id.trim().is_empty()
        {
            return Err(permanent("Matrix redaction target is invalid"));
        }
        if self.cursor.encrypted_rooms.contains(room_id) {
            return Err(permanent(
                "Matrix end-to-end encryption is not configured for this adapter",
            ));
        }
        let path = format!(
            "/_matrix/client/v3/rooms/{}/redact/{}/{}",
            percent_encode(room_id),
            percent_encode(target_event_id),
            percent_encode(transaction_id)
        );
        let response = self.send_json("PUT", &path, &json!({"reason": "Removed by Keith"}))?;
        let receipt: MatrixSendReceipt = serde_json::from_value(response)
            .map_err(|_| permanent("Matrix returned a malformed redaction receipt"))?;
        Ok(SendReceipt {
            platform_message_id: receipt.event_id,
            accepted_at: now(),
            duplicate_possible: false,
        })
    }

    /// Emits a bounded Matrix typing state.
    ///
    /// # Errors
    ///
    /// Returns classified validation or HTTP failures.
    pub fn set_typing(
        &mut self,
        room_id: &str,
        typing: bool,
        timeout_ms: u64,
    ) -> Result<(), AdapterFailure> {
        self.ensure_active()?;
        if !valid_matrix_room_id(room_id) || timeout_ms > 120_000 {
            return Err(permanent("Matrix typing request is invalid"));
        }
        let path = format!(
            "/_matrix/client/v3/rooms/{}/typing/{}",
            percent_encode(room_id),
            percent_encode(&self.config.user_id)
        );
        self.send_json(
            "PUT",
            &path,
            &json!({"typing": typing, "timeout": timeout_ms}),
        )?;
        Ok(())
    }

    /// Sends an `m.read` receipt for a concrete room event.
    ///
    /// # Errors
    ///
    /// Returns classified validation or HTTP failures.
    pub fn send_read_receipt(
        &mut self,
        room_id: &str,
        event_id: &str,
    ) -> Result<(), AdapterFailure> {
        let request = self.prepare_read_receipt(room_id, event_id)?;
        let path = request
            .url
            .strip_prefix(self.config.homeserver.trim_end_matches('/'))
            .ok_or_else(|| permanent("Matrix prepared receipt has an invalid homeserver"))?;
        self.send_json("POST", path, &json!({}))?;
        Ok(())
    }

    /// Prepares the exact non-secret Matrix read-receipt request.
    ///
    /// # Errors
    ///
    /// Returns a permanent failure for invalid room or event identity.
    pub fn prepare_read_receipt(
        &self,
        room_id: &str,
        event_id: &str,
    ) -> Result<PreparedHttpRequest, AdapterFailure> {
        self.ensure_active()?;
        if !valid_matrix_room_id(room_id) || !valid_matrix_event_id(event_id) {
            return Err(permanent("Matrix receipt target is invalid"));
        }
        let path = format!(
            "/_matrix/client/v3/rooms/{}/receipt/m.read/{}",
            percent_encode(room_id),
            percent_encode(event_id)
        );
        Ok(PreparedHttpRequest {
            method: "POST".to_owned(),
            url: format!("{}{}", self.config.homeserver.trim_end_matches('/'), path),
            content_type: "application/json".to_owned(),
            body: b"{}".to_vec(),
            idempotency_key: None,
        })
    }

    pub fn revoke(&mut self) {
        self.revoked = true;
        self.pending.clear();
        self.staged.clear();
        self.staged_bytes = 0;
        self.safe_error = Some("Matrix account was revoked".to_owned());
    }

    fn ensure_active(&self) -> Result<(), AdapterFailure> {
        if self.revoked {
            Err(permanent("Matrix account was revoked"))
        } else {
            Ok(())
        }
    }

    fn normalize_event(
        &mut self,
        room_id: &str,
        event: MatrixTimelineEvent,
    ) -> Result<Option<MatrixNormalizedEvent>, AdapterFailure> {
        let event_id = event
            .event_id
            .filter(|value| valid_matrix_event_id(value))
            .ok_or_else(|| permanent("Matrix timeline event has no valid event ID"))?;
        if self.seen.contains(&event_id) {
            return Ok(None);
        }
        let sender = event
            .sender
            .filter(|value| valid_matrix_user_id(value))
            .ok_or_else(|| permanent("Matrix timeline event has no valid sender"))?;
        let occurred_at = event
            .origin_server_ts
            .and_then(|value| i64::try_from(value).ok())
            .map_or_else(now, UtcTimestamp::from_unix_millis);
        self.remember(event_id.clone(), occurred_at);
        if sender == self.config.user_id {
            return Ok(None);
        }
        match event.event_type.as_str() {
            "m.room.encrypted" => {
                self.cursor.encrypted_rooms.insert(room_id.to_owned());
                Ok(Some(MatrixNormalizedEvent::UnsupportedEncrypted {
                    room_id: room_id.to_owned(),
                    event_id,
                }))
            }
            "m.room.redaction" => {
                let target_event_id = event
                    .redacts
                    .filter(|value| valid_matrix_event_id(value))
                    .ok_or_else(|| permanent("Matrix redaction has no valid target"))?;
                Ok(Some(MatrixNormalizedEvent::Redaction {
                    room_id: room_id.to_owned(),
                    sender,
                    event_id,
                    target_event_id,
                    occurred_at,
                }))
            }
            "m.reaction" => {
                let relation = relation(&event.content)?;
                if relation.rel_type.as_deref() != Some("m.annotation") {
                    return Ok(None);
                }
                Ok(Some(MatrixNormalizedEvent::Reaction {
                    room_id: room_id.to_owned(),
                    sender,
                    event_id,
                    target_event_id: required_event_id(relation.event_id)?,
                    key: relation
                        .key
                        .filter(|key| !key.trim().is_empty())
                        .ok_or_else(|| permanent("Matrix reaction has no key"))?,
                    occurred_at,
                }))
            }
            "m.room.message" => {
                self.normalize_room_message(room_id, sender, event_id, &event.content, occurred_at)
            }
            _ => Ok(None),
        }
    }

    fn normalize_room_message(
        &self,
        room_id: &str,
        sender: String,
        event_id: String,
        content: &Value,
        occurred_at: UtcTimestamp,
    ) -> Result<Option<MatrixNormalizedEvent>, AdapterFailure> {
        let relation = relation_optional(content)?;
        if relation
            .as_ref()
            .and_then(|value| value.rel_type.as_deref())
            == Some("m.replace")
        {
            let target_event_id = required_event_id(relation.and_then(|value| value.event_id))?;
            let text = content
                .get("m.new_content")
                .and_then(|value| value.get("body"))
                .and_then(Value::as_str)
                .or_else(|| content.get("body").and_then(Value::as_str))
                .filter(|value| !value.trim().is_empty())
                .ok_or_else(|| permanent("Matrix edit has no body"))?
                .to_owned();
            return Ok(Some(MatrixNormalizedEvent::Edit {
                room_id: room_id.to_owned(),
                sender,
                event_id,
                target_event_id,
                text,
                occurred_at,
            }));
        }
        let msgtype = content
            .get("msgtype")
            .and_then(Value::as_str)
            .unwrap_or("m.text");
        let text = content
            .get("body")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned();
        let attachments = normalize_media(msgtype, content, self.config.max_attachment_bytes)?;
        if text.trim().is_empty() && attachments.is_empty() {
            return Err(permanent("Matrix message has no supported content"));
        }
        let reply_target = content
            .get("m.relates_to")
            .and_then(|value| value.get("m.in_reply_to"))
            .and_then(|value| value.get("event_id"))
            .and_then(Value::as_str)
            .filter(|value| valid_matrix_event_id(value))
            .map(str::to_owned);
        let thread = relation
            .filter(|value| value.rel_type.as_deref() == Some("m.thread"))
            .and_then(|value| value.event_id)
            .filter(|value| valid_matrix_event_id(value));
        Ok(Some(MatrixNormalizedEvent::Message(Box::new(
            InboundMessage {
                channel: "matrix".to_owned(),
                external_account: self.config.external_account.clone(),
                conversation: room_id.to_owned(),
                thread,
                sender,
                message_id: event_id,
                reply_target,
                text,
                attachments,
                occurred_at,
                intent: InboundIntent::Prompt,
            },
        ))))
    }

    fn remember(&mut self, event_id: String, _occurred_at: UtcTimestamp) {
        self.seen.insert(event_id.clone());
        self.seen_order.push_back(event_id);
        while self.seen_order.len() > self.config.deduplication_capacity {
            if let Some(expired) = self.seen_order.pop_front() {
                self.seen.remove(&expired);
            }
        }
        self.cursor.recent_event_ids = self.seen_order.iter().cloned().collect();
    }

    fn send_message(&mut self, message: &OutboundMessage) -> Result<SendReceipt, AdapterFailure> {
        self.ensure_active()?;
        if message.route.channel != "matrix"
            || message.route.external_account != self.config.external_account
            || !valid_matrix_room_id(&message.route.conversation)
            || message.idempotency_key.trim().is_empty()
        {
            return Err(permanent(
                "Matrix delivery route belongs to another adapter account",
            ));
        }
        if self
            .cursor
            .encrypted_rooms
            .contains(&message.route.conversation)
        {
            return Err(permanent(
                "Matrix end-to-end encryption is not configured for this adapter",
            ));
        }
        if message.artifacts.len() > 1 {
            return Err(permanent(
                "Matrix common outbound messages support one media artifact",
            ));
        }
        let mut content = if let Some(artifact_id) = message.artifacts.first() {
            let upload = self
                .staged
                .get(artifact_id)
                .ok_or_else(|| permanent("Matrix artifact bytes were not staged"))?
                .clone();
            let content_uri = self.upload_media(&upload)?;
            json!({
                "msgtype": matrix_msgtype(&upload.media_type),
                "body": upload.file_name,
                "url": content_uri,
                "info": {
                    "mimetype": upload.media_type,
                    "size": upload.bytes.len(),
                }
            })
        } else {
            json!({"msgtype": "m.text", "body": message.text})
        };
        add_relation(&mut content, &message.route)?;
        let receipt = self.send_room_event(
            &message.route.conversation,
            "m.room.message",
            &message.idempotency_key,
            &content,
        )?;
        for id in &message.artifacts {
            if let Some(upload) = self.staged.remove(id) {
                self.staged_bytes = self.staged_bytes.saturating_sub(upload.bytes.len());
            }
        }
        Ok(receipt)
    }

    fn upload_media(&mut self, upload: &MatrixUpload) -> Result<String, AdapterFailure> {
        let path = format!(
            "/_matrix/media/v3/upload?filename={}",
            percent_encode(&upload.file_name)
        );
        let url = format!("{}{}", self.config.homeserver.trim_end_matches('/'), path);
        let mut response = self.access_token.with_bytes(|token| {
            let token = std::str::from_utf8(token)
                .map_err(|_| permanent("Matrix access token is not UTF-8"))?;
            self.http
                .post(&url)
                .header("Authorization", format!("Bearer {token}"))
                .header("Content-Type", &upload.media_type)
                .send(&upload.bytes)
                .map_err(|error| transport_error("Matrix", &error))
        })?;
        let status = response.status().as_u16();
        let bytes = response
            .body_mut()
            .with_config()
            .limit(u64::try_from(self.config.max_event_bytes).unwrap_or(u64::MAX))
            .read_to_vec()
            .map_err(|error| transport_error("Matrix", &error))?;
        match status {
            200..=299 => serde_json::from_slice::<MatrixMediaReceipt>(&bytes)
                .map(|receipt| receipt.content_uri)
                .map_err(|_| permanent("Matrix returned a malformed media receipt")),
            401 | 403 => Err(permanent("Matrix authentication or permission denied")),
            413 => Err(permanent("Matrix homeserver rejected oversized media")),
            429 => Err(matrix_rate_limit(&bytes)),
            500..=599 => Err(retryable("Matrix homeserver is temporarily unavailable")),
            _ => Err(permanent("Matrix rejected the media upload")),
        }
    }

    fn send_room_event(
        &mut self,
        room_id: &str,
        event_type: &str,
        transaction_id: &str,
        content: &Value,
    ) -> Result<SendReceipt, AdapterFailure> {
        let request = self.prepare_room_event(room_id, event_type, transaction_id, content)?;
        let path = request
            .url
            .strip_prefix(self.config.homeserver.trim_end_matches('/'))
            .ok_or_else(|| permanent("Matrix prepared request has an invalid homeserver"))?;
        let response = self.send_json("PUT", path, content)?;
        let receipt: MatrixSendReceipt = serde_json::from_value(response)
            .map_err(|_| permanent("Matrix returned a malformed send receipt"))?;
        Ok(SendReceipt {
            platform_message_id: receipt.event_id,
            accepted_at: now(),
            duplicate_possible: false,
        })
    }

    /// Prepares the exact non-secret idempotent Matrix room-event request.
    ///
    /// # Errors
    ///
    /// Returns a permanent failure for invalid room, event type, transaction, encryption state,
    /// or JSON encoding.
    pub fn prepare_room_event(
        &self,
        room_id: &str,
        event_type: &str,
        transaction_id: &str,
        content: &Value,
    ) -> Result<PreparedHttpRequest, AdapterFailure> {
        self.ensure_active()?;
        if !valid_matrix_room_id(room_id)
            || event_type.trim().is_empty()
            || transaction_id.trim().is_empty()
        {
            return Err(permanent("Matrix room event target is invalid"));
        }
        if self.cursor.encrypted_rooms.contains(room_id) {
            return Err(permanent(
                "Matrix end-to-end encryption is not configured for this adapter",
            ));
        }
        let path = format!(
            "/_matrix/client/v3/rooms/{}/send/{}/{}",
            percent_encode(room_id),
            percent_encode(event_type),
            percent_encode(transaction_id)
        );
        Ok(PreparedHttpRequest {
            method: "PUT".to_owned(),
            url: format!("{}{}", self.config.homeserver.trim_end_matches('/'), path),
            content_type: "application/json".to_owned(),
            body: serde_json::to_vec(content)
                .map_err(|_| permanent("Matrix request could not be encoded"))?,
            idempotency_key: Some(transaction_id.to_owned()),
        })
    }

    fn send_json(
        &self,
        method: &str,
        path: &str,
        content: &Value,
    ) -> Result<Value, AdapterFailure> {
        let url = format!("{}{}", self.config.homeserver.trim_end_matches('/'), path);
        let body = serde_json::to_vec(content)
            .map_err(|_| permanent("Matrix request could not be encoded"))?;
        let response = self.access_token.with_bytes(|token| {
            let token = std::str::from_utf8(token)
                .map_err(|_| permanent("Matrix access token is not UTF-8"))?;
            let request = match method {
                "POST" => self.http.post(&url),
                "PUT" => self.http.put(&url),
                _ => return Err(permanent("Matrix HTTP method is unsupported")),
            };
            request
                .header("Authorization", format!("Bearer {token}"))
                .header("Content-Type", "application/json")
                .send(&body)
                .map_err(|error| transport_error("Matrix", &error))
        })?;
        self.read_json_response(response, "Matrix request")
    }

    fn read_json_response(
        &self,
        mut response: ureq::http::Response<ureq::Body>,
        operation: &str,
    ) -> Result<Value, AdapterFailure> {
        let status = response.status().as_u16();
        let bytes = response
            .body_mut()
            .with_config()
            .limit(u64::try_from(self.config.max_event_bytes).unwrap_or(u64::MAX))
            .read_to_vec()
            .map_err(|error| transport_error("Matrix", &error))?;
        match status {
            200..=299 => serde_json::from_slice(&bytes)
                .map_err(|_| permanent(&format!("{operation} returned malformed JSON"))),
            401 | 403 => Err(permanent("Matrix authentication or permission denied")),
            404 => Err(permanent("Matrix destination does not exist")),
            429 => Err(matrix_rate_limit(&bytes)),
            500..=599 => Err(retryable("Matrix homeserver is temporarily unavailable")),
            _ => Err(permanent(&format!("{operation} was rejected"))),
        }
    }
}

impl ChannelAdapterV2 for MatrixAdapter {
    fn capabilities_v2(&self) -> ChannelCapabilitiesV2 {
        matrix_capabilities(&self.config)
    }

    fn receive_v2(&mut self) -> Result<ChannelEventV2, ChannelAdapterErrorV2> {
        let rich = self.receive_rich().map_err(ChannelAdapterErrorV2::from)?;
        let event = matrix_event_v2(&self.config.external_account, rich)?;
        event.validate(&self.capabilities_v2())?;
        Ok(event)
    }

    #[allow(clippy::too_many_lines)]
    fn execute_v2(
        &mut self,
        operation: &ChannelOperationV2,
    ) -> Result<ChannelOperationReceiptV2, ChannelAdapterErrorV2> {
        self.capabilities_v2()
            .require(operation.required_capability())?;
        match operation {
            ChannelOperationV2::SendMessage(message) => {
                validate_matrix_route(&self.config, &message.route)?;
                if !message.rich_content.is_empty() {
                    return Err(ChannelAdapterErrorV2::unsupported(
                        "Matrix formatted rich content is not enabled by this adapter",
                    ));
                }
                if self.cancelled.contains(&message.idempotency_key) {
                    return Err(v2_error(
                        ChannelAdapterErrorKindV2::Cancelled,
                        "Matrix delivery was cancelled before dispatch",
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
                    duplicate_possible: false,
                    metadata: std::collections::BTreeMap::new(),
                })
            }
            ChannelOperationV2::EditMessage {
                route,
                platform_message_id,
                text,
                rich_content,
            } => {
                validate_matrix_route(&self.config, route)?;
                if !rich_content.is_empty() {
                    return Err(ChannelAdapterErrorV2::unsupported(
                        "Matrix formatted edits are not enabled by this adapter",
                    ));
                }
                let transaction_id = stable_transaction_id(
                    "edit",
                    &[&route.conversation, platform_message_id, text],
                );
                let receipt = self
                    .send_edit(
                        &route.conversation,
                        platform_message_id,
                        text,
                        &transaction_id,
                    )
                    .map_err(ChannelAdapterErrorV2::from)?;
                Ok(operation_receipt(
                    transaction_id,
                    Some(&receipt.platform_message_id),
                    ChannelReceiptStateV2::Accepted,
                    false,
                ))
            }
            ChannelOperationV2::DeleteMessage {
                route,
                platform_message_id,
            } => {
                validate_matrix_route(&self.config, route)?;
                let transaction_id =
                    stable_transaction_id("redact", &[&route.conversation, platform_message_id]);
                let receipt = self
                    .send_redaction(&route.conversation, platform_message_id, &transaction_id)
                    .map_err(ChannelAdapterErrorV2::from)?;
                Ok(operation_receipt(
                    transaction_id,
                    Some(&receipt.platform_message_id),
                    ChannelReceiptStateV2::Accepted,
                    false,
                ))
            }
            ChannelOperationV2::AddReaction {
                route,
                platform_message_id,
                reaction,
            } => {
                validate_matrix_route(&self.config, route)?;
                let transaction_id = stable_transaction_id(
                    "reaction",
                    &[&route.conversation, platform_message_id, reaction],
                );
                let receipt = self
                    .send_reaction(
                        &route.conversation,
                        platform_message_id,
                        reaction,
                        &transaction_id,
                    )
                    .map_err(ChannelAdapterErrorV2::from)?;
                Ok(operation_receipt(
                    transaction_id,
                    Some(&receipt.platform_message_id),
                    ChannelReceiptStateV2::Accepted,
                    false,
                ))
            }
            ChannelOperationV2::RemoveReaction {
                route,
                platform_message_id,
                reaction,
            } => {
                validate_matrix_route(&self.config, route)?;
                let transaction_id = stable_transaction_id(
                    "unreact",
                    &[&route.conversation, platform_message_id, reaction],
                );
                let receipt = self
                    .send_redaction(&route.conversation, platform_message_id, &transaction_id)
                    .map_err(ChannelAdapterErrorV2::from)?;
                Ok(operation_receipt(
                    transaction_id,
                    Some(&receipt.platform_message_id),
                    ChannelReceiptStateV2::Accepted,
                    false,
                ))
            }
            ChannelOperationV2::SetTyping { route, active } => {
                validate_matrix_route(&self.config, route)?;
                self.set_typing(&route.conversation, *active, u64::from(*active) * 30_000)
                    .map_err(ChannelAdapterErrorV2::from)?;
                Ok(operation_receipt(
                    stable_transaction_id("typing", &[&route.conversation]),
                    None,
                    ChannelReceiptStateV2::Accepted,
                    false,
                ))
            }
            ChannelOperationV2::Cancel { cancellation_id } => {
                if cancellation_id.trim().is_empty() {
                    return Err(ChannelAdapterErrorV2::malformed(
                        "Matrix cancellation identity is empty",
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
        }
    }

    fn reconnect_v2(&mut self) -> Result<(), ChannelAdapterErrorV2> {
        self.ensure_active().map_err(ChannelAdapterErrorV2::from)?;
        let url = format!(
            "{}/_matrix/client/versions",
            self.config.homeserver.trim_end_matches('/')
        );
        let response = self
            .http
            .get(&url)
            .call()
            .map_err(|error| transport_error("Matrix", &error))?;
        self.read_json_response(response, "Matrix versions")?;
        self.safe_error = None;
        Ok(())
    }

    fn reconnect_cursor_v2(&self) -> Option<ReconnectCursorV2> {
        self.cursor
            .next_batch
            .as_ref()
            .zip(self.cursor.last_sync_at)
            .map(|(value, observed_at)| ReconnectCursorV2 {
                value: value.clone(),
                observed_at,
            })
    }
}

impl ChannelAdapter for MatrixAdapter {
    fn features(&self) -> AdapterFeatures {
        AdapterFeatures {
            capabilities: BTreeSet::from([
                AdapterCapability::Attachments,
                AdapterCapability::Threads,
                AdapterCapability::IdempotentSend,
                AdapterCapability::Reconnect,
            ]),
            max_attachment_bytes: self.config.max_attachment_bytes,
            requests_per_minute: None,
        }
    }

    fn receive(&mut self) -> Result<AdapterEvent, AdapterFailure> {
        loop {
            let event = self.receive_v2().map_err(adapter_failure_from_v2)?;
            if let Some(event) = event_v2_to_v1("matrix", event) {
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
                metadata: std::collections::BTreeMap::new(),
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
struct MatrixSyncResponse {
    next_batch: String,
    #[serde(default)]
    rooms: MatrixRooms,
}

#[derive(Debug, Default, Deserialize)]
struct MatrixRooms {
    #[serde(default)]
    join: std::collections::BTreeMap<String, MatrixJoinedRoom>,
}

#[derive(Debug, Default, Deserialize)]
struct MatrixJoinedRoom {
    #[serde(default)]
    state: MatrixEventList,
    #[serde(default)]
    timeline: MatrixEventList,
}

#[derive(Debug, Default, Deserialize)]
struct MatrixEventList {
    #[serde(default)]
    events: Vec<MatrixTimelineEvent>,
}

#[derive(Debug, Deserialize)]
struct MatrixTimelineEvent {
    #[serde(rename = "type")]
    event_type: String,
    event_id: Option<String>,
    sender: Option<String>,
    origin_server_ts: Option<u64>,
    #[serde(default)]
    content: Value,
    redacts: Option<String>,
}

#[derive(Clone, Debug, Deserialize)]
struct MatrixRelation {
    rel_type: Option<String>,
    event_id: Option<String>,
    key: Option<String>,
}

#[derive(Debug, Deserialize)]
struct MatrixSendReceipt {
    event_id: String,
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct MatrixWhoAmI {
    user_id: String,
}

#[derive(Debug, Deserialize)]
struct MatrixMediaReceipt {
    content_uri: String,
}

fn relation(content: &Value) -> Result<MatrixRelation, AdapterFailure> {
    relation_optional(content)?.ok_or_else(|| permanent("Matrix event has no relation"))
}

fn relation_optional(content: &Value) -> Result<Option<MatrixRelation>, AdapterFailure> {
    content
        .get("m.relates_to")
        .map(|value| {
            serde_json::from_value(value.clone())
                .map_err(|_| permanent("Matrix event relation is malformed"))
        })
        .transpose()
}

fn required_event_id(value: Option<String>) -> Result<String, AdapterFailure> {
    value
        .filter(|value| valid_matrix_event_id(value))
        .ok_or_else(|| permanent("Matrix relation has no valid event ID"))
}

fn normalize_media(
    msgtype: &str,
    content: &Value,
    max_attachment_bytes: u64,
) -> Result<Vec<Attachment>, AdapterFailure> {
    if !matches!(msgtype, "m.file" | "m.image" | "m.audio" | "m.video") {
        return Ok(Vec::new());
    }
    let content_uri = content
        .get("url")
        .and_then(Value::as_str)
        .filter(|value| value.starts_with("mxc://"))
        .ok_or_else(|| permanent("Matrix media event has no valid content URI"))?;
    let info = content.get("info").and_then(Value::as_object);
    let byte_length = info
        .and_then(|value| value.get("size"))
        .and_then(Value::as_u64)
        .unwrap_or(0);
    if byte_length > max_attachment_bytes {
        return Err(permanent("Matrix media exceeds the configured limit"));
    }
    Ok(vec![Attachment {
        id: content_uri.to_owned(),
        file_name: content
            .get("filename")
            .or_else(|| content.get("body"))
            .and_then(Value::as_str)
            .unwrap_or("attachment")
            .to_owned(),
        media_type: info
            .and_then(|value| value.get("mimetype"))
            .and_then(Value::as_str)
            .unwrap_or("application/octet-stream")
            .to_owned(),
        byte_length,
        artifact_id: None,
        download_url: Some(content_uri.to_owned()),
        staging_file: None,
        sha256: None,
    }])
}

fn add_relation(
    content: &mut Value,
    route: &keith_channel_core::ReplyRoute,
) -> Result<(), AdapterFailure> {
    let object = content
        .as_object_mut()
        .ok_or_else(|| permanent("Matrix message content is malformed"))?;
    let mut relation = Map::new();
    if let Some(thread) = &route.thread {
        if !valid_matrix_event_id(thread) {
            return Err(permanent("Matrix thread event ID is malformed"));
        }
        relation.insert("rel_type".to_owned(), Value::String("m.thread".to_owned()));
        relation.insert("event_id".to_owned(), Value::String(thread.clone()));
    }
    if let Some(reply) = &route.reply_to_message {
        if !valid_matrix_event_id(reply) {
            return Err(permanent("Matrix reply event ID is malformed"));
        }
        relation.insert("m.in_reply_to".to_owned(), json!({"event_id": reply}));
    }
    if !relation.is_empty() {
        object.insert("m.relates_to".to_owned(), Value::Object(relation));
    }
    Ok(())
}

fn matrix_msgtype(media_type: &str) -> &'static str {
    if media_type.starts_with("image/") {
        "m.image"
    } else if media_type.starts_with("audio/") {
        "m.audio"
    } else if media_type.starts_with("video/") {
        "m.video"
    } else {
        "m.file"
    }
}

fn matrix_rate_limit(bytes: &[u8]) -> AdapterFailure {
    let retry_after_ms = serde_json::from_slice::<Value>(bytes)
        .ok()
        .and_then(|body| body.get("retry_after_ms").and_then(Value::as_u64));
    rate_limited("Matrix rate limit reached", retry_after_ms)
}

#[allow(clippy::too_many_lines)]
fn matrix_event_v2(
    account_id: &str,
    event: MatrixNormalizedEvent,
) -> Result<ChannelEventV2, ChannelAdapterErrorV2> {
    let (event_id, kind) = match event {
        MatrixNormalizedEvent::Message(message) => {
            let event_id = message.message_id.clone();
            let thread_id = message.thread.clone();
            let reply_to_message_id = message.reply_target.clone();
            let attachments = message
                .attachments
                .into_iter()
                .map(|attachment| {
                    let kind = if attachment.media_type.starts_with("image/") {
                        ChannelAttachmentKindV2::Image
                    } else if attachment.media_type.starts_with("audio/") {
                        ChannelAttachmentKindV2::Audio
                    } else if attachment.media_type.starts_with("video/") {
                        ChannelAttachmentKindV2::Video
                    } else {
                        ChannelAttachmentKindV2::File
                    };
                    ChannelAttachmentV2 {
                        attachment,
                        kind,
                        duration_ms: None,
                        metadata: std::collections::BTreeMap::new(),
                    }
                })
                .collect();
            (
                event_id,
                ChannelEventKindV2::MessageCreated(ChannelMessageV2 {
                    message_id: message.message_id,
                    account_id: account_id.to_owned(),
                    conversation: ChannelConversationV2 {
                        platform_id: message.conversation,
                        kind: ChannelConversationKindV2::Channel,
                        thread_id,
                        reply_to_message_id,
                    },
                    sender: ChannelIdentityV2 {
                        platform_id: message.sender,
                        display_name: None,
                        is_bot: false,
                    },
                    text: message.text,
                    attachments,
                    rich_content: Vec::new(),
                    mentions: Vec::new(),
                    occurred_at: message.occurred_at,
                    metadata: std::collections::BTreeMap::from([(
                        "platform".to_owned(),
                        "matrix".to_owned(),
                    )]),
                }),
            )
        }
        MatrixNormalizedEvent::Edit {
            room_id,
            sender,
            event_id,
            target_event_id,
            text,
            occurred_at,
        } => {
            let envelope_id = event_id.clone();
            (
                envelope_id,
                ChannelEventKindV2::MessageEdited(ChannelMessageEditV2 {
                    message_id: target_event_id,
                    account_id: account_id.to_owned(),
                    conversation: matrix_conversation(room_id),
                    editor: ChannelIdentityV2 {
                        platform_id: sender,
                        display_name: None,
                        is_bot: false,
                    },
                    text,
                    occurred_at,
                    metadata: std::collections::BTreeMap::from([(
                        "matrix_edit_event_id".to_owned(),
                        event_id,
                    )]),
                }),
            )
        }
        MatrixNormalizedEvent::Reaction {
            room_id,
            sender,
            event_id,
            target_event_id,
            key,
            occurred_at,
        } => {
            let envelope_id = event_id.clone();
            (
                envelope_id,
                ChannelEventKindV2::Reaction(ChannelReactionV2 {
                    message_id: target_event_id,
                    account_id: account_id.to_owned(),
                    conversation: matrix_conversation(room_id),
                    actor: ChannelIdentityV2 {
                        platform_id: sender,
                        display_name: None,
                        is_bot: false,
                    },
                    reaction: key,
                    action: ChannelReactionActionV2::Added,
                    occurred_at,
                    metadata: std::collections::BTreeMap::from([(
                        "matrix_reaction_event_id".to_owned(),
                        event_id,
                    )]),
                }),
            )
        }
        MatrixNormalizedEvent::Redaction {
            room_id,
            sender,
            event_id,
            target_event_id,
            occurred_at,
        } => {
            let envelope_id = event_id.clone();
            (
                envelope_id,
                ChannelEventKindV2::MessageDeleted(ChannelMessageDeleteV2 {
                    message_id: target_event_id,
                    account_id: account_id.to_owned(),
                    conversation: matrix_conversation(room_id),
                    actor: Some(ChannelIdentityV2 {
                        platform_id: sender,
                        display_name: None,
                        is_bot: false,
                    }),
                    occurred_at,
                    metadata: std::collections::BTreeMap::from([(
                        "matrix_redaction_event_id".to_owned(),
                        event_id,
                    )]),
                }),
            )
        }
        MatrixNormalizedEvent::UnsupportedEncrypted { .. } => {
            return Err(v2_error(
                ChannelAdapterErrorKindV2::UnsupportedFeature,
                "Matrix encrypted event is unsupported without an encryption provider",
            ));
        }
    };
    Ok(ChannelEventV2 {
        contract: ChannelContractVersion::new(2, 0),
        event_id,
        delivery_attempt: 1,
        event: kind,
        metadata: std::collections::BTreeMap::new(),
    })
}

fn matrix_conversation(room_id: String) -> ChannelConversationV2 {
    ChannelConversationV2 {
        platform_id: room_id,
        kind: ChannelConversationKindV2::Channel,
        thread_id: None,
        reply_to_message_id: None,
    }
}

fn matrix_capabilities(config: &MatrixConfig) -> ChannelCapabilitiesV2 {
    let unsupported = |reason: &str| ChannelCapabilitySupportV2::Unsupported {
        safe_reason: reason.to_owned(),
    };
    let mut declarations = ChannelCapabilityV2::ALL
        .into_iter()
        .map(|capability| (capability, ChannelCapabilitySupportV2::Supported))
        .collect::<std::collections::BTreeMap<_, _>>();
    declarations.insert(
        ChannelCapabilityV2::Mentions,
        unsupported("Matrix mention metadata is not projected by this adapter"),
    );
    declarations.insert(
        ChannelCapabilityV2::Commands,
        unsupported("Matrix commands are not interpreted by the adapter"),
    );
    declarations.insert(
        ChannelCapabilityV2::Voice,
        unsupported("Matrix voice messages are represented as audio media"),
    );
    declarations.insert(
        ChannelCapabilityV2::RichContent,
        unsupported("Matrix formatted bodies are not enabled by this adapter"),
    );
    declarations.insert(
        ChannelCapabilityV2::DeliveryReceipts,
        unsupported("Matrix delivery receipt events are not projected by this adapter"),
    );
    ChannelCapabilitiesV2 {
        contract: ChannelContractVersion::new(2, 0),
        declarations,
        max_event_bytes: u64::try_from(config.max_event_bytes).unwrap_or(u64::MAX),
        max_attachment_bytes: config.max_attachment_bytes,
        max_attachments: 1,
        max_rich_content_bytes: u64::try_from(config.max_event_bytes).unwrap_or(u64::MAX),
        requests_per_minute: None,
    }
}

fn validate_matrix_route(
    config: &MatrixConfig,
    route: &keith_channel_core::ReplyRoute,
) -> Result<(), ChannelAdapterErrorV2> {
    if route.channel != "matrix"
        || route.external_account != config.external_account
        || !valid_matrix_room_id(&route.conversation)
    {
        return Err(v2_error(
            ChannelAdapterErrorKindV2::Permission,
            "Matrix delivery route belongs to another adapter account",
        ));
    }
    Ok(())
}

fn stable_transaction_id(prefix: &str, fields: &[&str]) -> String {
    let mut digest = 0xcbf2_9ce4_8422_2325_u64;
    for field in fields {
        for byte in field.as_bytes() {
            digest ^= u64::from(*byte);
            digest = digest.wrapping_mul(0x0000_0100_0000_01b3);
        }
        digest ^= 0xff;
        digest = digest.wrapping_mul(0x0000_0100_0000_01b3);
    }
    format!("keith-{prefix}-{digest:016x}")
}

fn valid_matrix_room_id(value: &str) -> bool {
    value.starts_with('!') && value.contains(':') && !value.contains(char::is_whitespace)
}

fn valid_matrix_event_id(value: &str) -> bool {
    value.starts_with('$') && value.len() > 1 && !value.contains(char::is_whitespace)
}

fn valid_matrix_user_id(value: &str) -> bool {
    value.starts_with('@') && value.contains(':') && !value.contains(char::is_whitespace)
}
