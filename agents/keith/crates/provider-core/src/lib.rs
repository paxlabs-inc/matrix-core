#![forbid(unsafe_code)]

mod embeddings;

pub use embeddings::{
    EMBEDDING_CONTRACT_VERSION, EmbeddingContractError, EmbeddingDescriptor, EmbeddingDistance,
    EmbeddingInput, EmbeddingLimits, EmbeddingNormalization, EmbeddingProvider, EmbeddingRequest,
    EmbeddingResponse, EmbeddingRole, EmbeddingSpaceIdentity, EmbeddingUsage, EmbeddingVector,
};

use std::collections::BTreeMap;
use std::fmt::{self, Debug, Display};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Weak};

use keith_agent_types::{EntityId, EntryId, SessionId, ToolCallId, TurnId};
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ModelDescriptor {
    pub provider: String,
    pub id: String,
    pub display_name: String,
    pub context_tokens: Option<u64>,
    pub output_tokens: Option<u64>,
    pub supports_tools: bool,
    pub supports_reasoning: bool,
    pub supports_vision: bool,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MessageRole {
    System,
    User,
    Assistant,
    Tool,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "type")]
pub enum ContentBlock {
    Text {
        text: String,
    },
    Image {
        media_type: String,
        data: String,
    },
    ToolCall {
        id: ToolCallId,
        name: String,
        arguments: serde_json::Value,
    },
    ToolResult {
        call_id: ToolCallId,
        content: String,
        is_error: bool,
    },
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Message {
    pub role: MessageRole,
    pub content: Vec<ContentBlock>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ContextProvenance {
    SystemPolicy,
    DeveloperPolicy,
    SessionContract,
    ActiveGoal,
    DurableMemory,
    RetrievedMemory,
    RelationshipContext,
    MemoryWriteAuthority,
    RetrievedKnowledge,
    CompactionSummary,
    UserIngress,
    AssistantCommentary,
    AssistantFinal,
    ToolCall,
    ToolResult,
    ControllerGuidance,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PersistPolicy {
    Never,
    Session,
    Durable,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ModelVisibility {
    Hidden,
    Visible,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ContextRecord {
    pub session_id: SessionId,
    pub turn_id: TurnId,
    pub entry_id: EntryId,
    pub source_id: String,
    pub provenance: ContextProvenance,
    pub current_turn: bool,
    pub persist_policy: PersistPolicy,
    pub model_visibility: ModelVisibility,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RequestContext {
    pub system: Vec<ContextRecord>,
    pub messages: Vec<Vec<ContextRecord>>,
    pub active_user_entry_id: EntryId,
    pub verbatim_last_user_message: String,
}

#[derive(Clone, Debug, Eq, Error, PartialEq)]
pub enum ContextContractError {
    #[error("system context metadata does not align with system content")]
    SystemAlignment,
    #[error("message context metadata does not align with provider messages")]
    MessageAlignment,
    #[error("provider role=user if and only if provenance=user_ingress")]
    UserRoleProvenance,
    #[error("provider tool content must retain tool provenance")]
    ToolProvenance,
    #[error("active user entry is missing, non-user, or not current")]
    ActiveUserEntry,
    #[error("verbatim last user message does not match the active user entry")]
    VerbatimUserMessage,
    #[error("hidden context was included in provider-visible content")]
    HiddenVisible,
}

impl RequestContext {
    /// Verifies that every provider-visible item retains its typed provenance and identity.
    ///
    /// # Errors
    ///
    /// Returns a context-contract error when metadata and provider content diverge.
    pub fn validate(
        &self,
        system: &[ContentBlock],
        messages: &[Message],
    ) -> Result<(), ContextContractError> {
        if self.system.len() != system.len() {
            return Err(ContextContractError::SystemAlignment);
        }
        if self.messages.len() != messages.len()
            || self
                .messages
                .iter()
                .zip(messages)
                .any(|(records, message)| records.len() != message.content.len())
        {
            return Err(ContextContractError::MessageAlignment);
        }
        if self
            .system
            .iter()
            .chain(self.messages.iter().flatten())
            .any(|record| record.model_visibility == ModelVisibility::Hidden)
        {
            return Err(ContextContractError::HiddenVisible);
        }
        if self
            .system
            .iter()
            .any(|record| record.provenance == ContextProvenance::UserIngress)
        {
            return Err(ContextContractError::UserRoleProvenance);
        }
        for (message, records) in messages.iter().zip(&self.messages) {
            for (content, record) in message.content.iter().zip(records) {
                if (message.role == MessageRole::User)
                    != (record.provenance == ContextProvenance::UserIngress)
                {
                    return Err(ContextContractError::UserRoleProvenance);
                }
                if matches!(content, ContentBlock::ToolResult { .. })
                    != (record.provenance == ContextProvenance::ToolResult)
                    || matches!(content, ContentBlock::ToolCall { .. })
                        != (record.provenance == ContextProvenance::ToolCall)
                {
                    return Err(ContextContractError::ToolProvenance);
                }
            }
        }
        let active = self
            .messages
            .iter()
            .enumerate()
            .flat_map(|(message_index, records)| {
                records
                    .iter()
                    .enumerate()
                    .map(move |(content_index, record)| (message_index, content_index, record))
            })
            .find(|(_, _, record)| record.entry_id == self.active_user_entry_id)
            .ok_or(ContextContractError::ActiveUserEntry)?;
        if active.2.provenance != ContextProvenance::UserIngress || !active.2.current_turn {
            return Err(ContextContractError::ActiveUserEntry);
        }
        let text = match &messages[active.0].content[active.1] {
            ContentBlock::Text { text } => text,
            ContentBlock::Image { .. }
            | ContentBlock::ToolCall { .. }
            | ContentBlock::ToolResult { .. } => {
                return Err(ContextContractError::VerbatimUserMessage);
            }
        };
        if text != &self.verbatim_last_user_message {
            return Err(ContextContractError::VerbatimUserMessage);
        }
        Ok(())
    }

    pub fn synthetic(system: &[ContentBlock], messages: &[Message]) -> Self {
        let session_id = SessionId::new();
        let turn_id = TurnId::new();
        let mut active_user_entry_id = EntryId::new();
        let mut verbatim_last_user_message = String::new();
        let system = system
            .iter()
            .map(|_| {
                context_record(
                    &session_id,
                    &turn_id,
                    ContextProvenance::SystemPolicy,
                    "synthetic_system",
                    false,
                )
            })
            .collect();
        let mut message_records: Vec<Vec<ContextRecord>> = Vec::with_capacity(messages.len());
        for message in messages {
            let mut records = Vec::with_capacity(message.content.len());
            for content in &message.content {
                let provenance = match content {
                    ContentBlock::ToolCall { .. } => ContextProvenance::ToolCall,
                    ContentBlock::ToolResult { .. } => ContextProvenance::ToolResult,
                    ContentBlock::Text { .. } | ContentBlock::Image { .. }
                        if message.role == MessageRole::User =>
                    {
                        ContextProvenance::UserIngress
                    }
                    ContentBlock::Text { .. } | ContentBlock::Image { .. } => {
                        ContextProvenance::AssistantCommentary
                    }
                };
                let mut record = context_record(
                    &session_id,
                    &turn_id,
                    provenance,
                    "synthetic_message",
                    false,
                );
                if provenance == ContextProvenance::UserIngress {
                    active_user_entry_id = record.entry_id.clone();
                    record.current_turn = true;
                    if let ContentBlock::Text { text } = content {
                        verbatim_last_user_message.clone_from(text);
                    }
                    for earlier in message_records.iter_mut().flatten() {
                        earlier.current_turn = false;
                    }
                }
                records.push(record);
            }
            message_records.push(records);
        }
        Self {
            system,
            messages: message_records,
            active_user_entry_id,
            verbatim_last_user_message,
        }
    }
}

fn context_record(
    session_id: &SessionId,
    turn_id: &TurnId,
    provenance: ContextProvenance,
    source_id: &str,
    current_turn: bool,
) -> ContextRecord {
    ContextRecord {
        session_id: session_id.clone(),
        turn_id: turn_id.clone(),
        entry_id: EntryId::new(),
        source_id: source_id.into(),
        provenance,
        current_turn,
        persist_policy: PersistPolicy::Session,
        model_visibility: ModelVisibility::Visible,
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ToolBehavior {
    ReadOnly,
    StateChanging,
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ModelRequestPurpose {
    #[default]
    Primary,
    Classification,
    Summarization,
    Review,
    Vision,
    MemoryScout,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ToolDefinition {
    pub name: String,
    pub description: String,
    pub input_schema: serde_json::Value,
    pub behavior: ToolBehavior,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ModelRequest {
    pub request_id: EntityId,
    #[serde(default)]
    pub purpose: ModelRequestPurpose,
    pub model: String,
    pub system: Vec<ContentBlock>,
    pub messages: Vec<Message>,
    pub tools: Vec<ToolDefinition>,
    pub max_output_tokens: Option<u32>,
    pub temperature: Option<f32>,
    pub reasoning_effort: Option<String>,
    pub context: RequestContext,
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Usage {
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub cached_input_tokens: u64,
}

impl Usage {
    pub const fn total_tokens(self) -> u64 {
        self.input_tokens.saturating_add(self.output_tokens)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum StopReason {
    EndTurn,
    ToolUse,
    MaxTokens,
    ContentRejected,
    Cancelled,
    Other,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "event")]
pub enum ModelEvent {
    Started {
        provider_request_id: Option<String>,
        model: String,
    },
    TextDelta {
        text: String,
    },
    ReasoningDelta {
        text: String,
    },
    ToolCallStarted {
        id: ToolCallId,
        name: String,
    },
    ToolCallArgumentsDelta {
        id: ToolCallId,
        delta: String,
    },
    ToolCallCompleted {
        id: ToolCallId,
        name: String,
        arguments: serde_json::Value,
    },
    Usage {
        usage: Usage,
    },
    Finished {
        reason: StopReason,
    },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ProviderErrorKind {
    Authentication,
    InvalidRequest,
    ContextOverflow,
    ContentRejected,
    Cancelled,
    RateLimited,
    Timeout,
    Unavailable,
    MalformedResponse,
    Internal,
}

#[derive(Clone, Debug, Eq, Error, PartialEq, Serialize, Deserialize)]
#[error("{kind:?}: {message}")]
#[serde(deny_unknown_fields)]
pub struct ProviderError {
    pub kind: ProviderErrorKind,
    pub message: String,
    pub retry_after_ms: Option<u64>,
    pub provider_status: Option<u16>,
}

impl ProviderError {
    pub fn new(kind: ProviderErrorKind, message: impl Into<String>) -> Self {
        Self {
            kind,
            message: message.into(),
            retry_after_ms: None,
            provider_status: None,
        }
    }

    pub const fn allows_retry_or_fallback(&self) -> bool {
        matches!(
            self.kind,
            ProviderErrorKind::RateLimited
                | ProviderErrorKind::Timeout
                | ProviderErrorKind::Unavailable
        )
    }
}

pub struct ProviderCredential {
    bytes: Vec<u8>,
}

impl ProviderCredential {
    /// # Errors
    ///
    /// Returns an authentication error for empty credentials or header injection bytes.
    pub fn new(value: impl Into<Vec<u8>>) -> Result<Self, ProviderError> {
        let bytes = value.into();
        if bytes.is_empty() || bytes.iter().any(|byte| *byte == b'\r' || *byte == b'\n') {
            Err(ProviderError::new(
                ProviderErrorKind::Authentication,
                "provider credential is empty or contains a line break",
            ))
        } else {
            Ok(Self { bytes })
        }
    }

    /// # Errors
    ///
    /// Returns an authentication error when the credential is not valid UTF-8.
    pub fn expose_utf8(&self) -> Result<&str, ProviderError> {
        std::str::from_utf8(&self.bytes).map_err(|_| {
            ProviderError::new(
                ProviderErrorKind::Authentication,
                "provider credential is not valid UTF-8",
            )
        })
    }
}

impl Debug for ProviderCredential {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("ProviderCredential([REDACTED])")
    }
}

impl Drop for ProviderCredential {
    fn drop(&mut self) {
        self.bytes.fill(0);
    }
}

#[derive(Debug)]
struct CancellationState {
    cancelled: AtomicBool,
    parent: Option<Weak<CancellationState>>,
}

#[derive(Clone, Debug)]
pub struct CancellationToken {
    state: Arc<CancellationState>,
}

impl Default for CancellationToken {
    fn default() -> Self {
        Self {
            state: Arc::new(CancellationState {
                cancelled: AtomicBool::new(false),
                parent: None,
            }),
        }
    }
}

impl CancellationToken {
    pub fn cancel(&self) {
        self.state.cancelled.store(true, Ordering::Release);
    }

    #[must_use]
    pub fn child_token(&self) -> Self {
        Self {
            state: Arc::new(CancellationState {
                cancelled: AtomicBool::new(false),
                parent: Some(Arc::downgrade(&self.state)),
            }),
        }
    }

    pub fn is_cancelled(&self) -> bool {
        let mut current = Some(Arc::clone(&self.state));
        while let Some(state) = current {
            if state.cancelled.load(Ordering::Acquire) {
                return true;
            }
            current = state.parent.as_ref().and_then(Weak::upgrade);
        }
        false
    }

    /// # Errors
    ///
    /// Returns a cancelled provider error after cancellation is requested.
    pub fn check(&self) -> Result<(), ProviderError> {
        if self.is_cancelled() {
            Err(ProviderError::new(
                ProviderErrorKind::Cancelled,
                "provider request was cancelled",
            ))
        } else {
            Ok(())
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum StreamControl {
    Continue,
    Cancel,
}

pub trait ModelEventSink {
    /// # Errors
    ///
    /// Returns an error when the consumer cannot accept an event.
    fn emit(&mut self, event: ModelEvent) -> Result<StreamControl, ProviderError>;
}

impl<F> ModelEventSink for F
where
    F: FnMut(ModelEvent) -> Result<StreamControl, ProviderError>,
{
    fn emit(&mut self, event: ModelEvent) -> Result<StreamControl, ProviderError> {
        self(event)
    }
}

pub trait ModelProvider: Send + Sync {
    fn provider_id(&self) -> &str;

    /// # Errors
    ///
    /// Returns a typed provider error when discovery fails.
    fn list_models(
        &self,
        credential: &ProviderCredential,
    ) -> Result<Vec<ModelDescriptor>, ProviderError>;

    /// # Errors
    ///
    /// Returns a typed provider error for transport, stream, cancellation, or provider failures.
    fn stream(
        &self,
        request: &ModelRequest,
        credential: &ProviderCredential,
        cancellation: &CancellationToken,
        sink: &mut dyn ModelEventSink,
    ) -> Result<Usage, ProviderError>;

    /// # Errors
    ///
    /// Returns a typed provider error when token counting fails.
    fn count_tokens(
        &self,
        request: &ModelRequest,
        credential: &ProviderCredential,
    ) -> Result<u64, ProviderError>;

    /// # Errors
    ///
    /// Returns a typed provider error when cancellation cannot be requested.
    fn cancel(&self, request_id: &EntityId) -> Result<(), ProviderError>;
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct ToolCallAssembly {
    calls: BTreeMap<ToolCallId, (String, String)>,
}

impl ToolCallAssembly {
    pub fn start(&mut self, id: ToolCallId, name: impl Into<String>) {
        self.calls.entry(id).or_insert((name.into(), String::new()));
    }

    /// # Errors
    ///
    /// Returns a malformed-response error when the provider did not start the tool call.
    pub fn push_arguments(&mut self, id: &ToolCallId, delta: &str) -> Result<(), ProviderError> {
        let Some((_, arguments)) = self.calls.get_mut(id) else {
            return Err(ProviderError::new(
                ProviderErrorKind::MalformedResponse,
                "tool arguments arrived before the tool call started",
            ));
        };
        arguments.push_str(delta);
        Ok(())
    }

    /// # Errors
    ///
    /// Returns a malformed-response error for missing or invalid tool arguments.
    pub fn finish(
        &mut self,
        id: &ToolCallId,
    ) -> Result<(String, serde_json::Value), ProviderError> {
        let (name, arguments) = self.calls.remove(id).ok_or_else(|| {
            ProviderError::new(
                ProviderErrorKind::MalformedResponse,
                "provider completed an unknown tool call",
            )
        })?;
        let arguments = serde_json::from_str(&arguments).map_err(|error| {
            ProviderError::new(
                ProviderErrorKind::MalformedResponse,
                format!("tool arguments are invalid JSON: {error}"),
            )
        })?;
        Ok((name, arguments))
    }
}

/// # Errors
///
/// Returns an error when the normalized request cannot be serialized or counted.
pub fn approximate_token_count(request: &ModelRequest) -> Result<u64, ProviderError> {
    let bytes = serde_json::to_vec(request)
        .map_err(|error| ProviderError::new(ProviderErrorKind::Internal, error.to_string()))?
        .len();
    let tokens = bytes.saturating_add(3) / 4;
    u64::try_from(tokens).map_err(|_| {
        ProviderError::new(
            ProviderErrorKind::InvalidRequest,
            "request is too large to count tokens",
        )
    })
}

/// # Errors
///
/// Returns an invalid-request error for missing model/messages or malformed tool schemas.
pub fn validate_request(request: &ModelRequest) -> Result<(), ProviderError> {
    if request.model.trim().is_empty() || request.messages.is_empty() {
        return Err(ProviderError::new(
            ProviderErrorKind::InvalidRequest,
            "model and at least one message are required",
        ));
    }
    if request
        .tools
        .iter()
        .any(|tool| tool.name.trim().is_empty() || !tool.input_schema.is_object())
    {
        return Err(ProviderError::new(
            ProviderErrorKind::InvalidRequest,
            "tool names and object input schemas are required",
        ));
    }
    Ok(())
}

/// # Errors
///
/// Returns a cancellation or consumer error when the stream cannot continue.
pub fn emit(
    sink: &mut dyn ModelEventSink,
    cancellation: &CancellationToken,
    event: ModelEvent,
) -> Result<(), ProviderError> {
    cancellation.check()?;
    if sink.emit(event)? == StreamControl::Cancel {
        cancellation.cancel();
        cancellation.check()?;
    }
    Ok(())
}

pub fn classify_http_status(status: u16, message: impl Into<String>) -> ProviderError {
    let kind = match status {
        400 | 404 | 405 | 409 | 422 => ProviderErrorKind::InvalidRequest,
        401 | 403 => ProviderErrorKind::Authentication,
        429 => ProviderErrorKind::RateLimited,
        408 | 504 => ProviderErrorKind::Timeout,
        500..=599 => ProviderErrorKind::Unavailable,
        _ => ProviderErrorKind::Internal,
    };
    let mut error = ProviderError::new(kind, message);
    error.provider_status = Some(status);
    error
}

pub fn redacted_error(error: impl Display) -> ProviderError {
    ProviderError::new(ProviderErrorKind::Internal, error.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn request() -> ModelRequest {
        let system = Vec::new();
        let messages = vec![Message {
            role: MessageRole::User,
            content: vec![ContentBlock::Text {
                text: "hello".into(),
            }],
        }];
        let context = RequestContext::synthetic(&system, &messages);
        ModelRequest {
            request_id: EntityId::new(),
            purpose: ModelRequestPurpose::Primary,
            model: "model-a".into(),
            system,
            messages,
            tools: Vec::new(),
            max_output_tokens: Some(100),
            temperature: None,
            reasoning_effort: None,
            context,
        }
    }

    #[test]
    fn credential_debug_and_errors_never_expose_secret_value() {
        let credential = ProviderCredential::new("highly-secret").unwrap();
        assert_eq!(format!("{credential:?}"), "ProviderCredential([REDACTED])");
        assert!(
            !redacted_error("transport failed")
                .to_string()
                .contains("highly-secret")
        );
    }

    #[test]
    fn context_contract_enforces_user_ingress_and_tool_roles() {
        let mut request = request();
        assert!(
            request
                .context
                .validate(&request.system, &request.messages)
                .is_ok()
        );
        request.context.messages[0][0].provenance = ContextProvenance::ControllerGuidance;
        assert_eq!(
            request.context.validate(&request.system, &request.messages),
            Err(ContextContractError::UserRoleProvenance)
        );

        for (encoded, expected) in [
            ("compaction_summary", ContextProvenance::CompactionSummary),
            ("durable_memory", ContextProvenance::DurableMemory),
            ("retrieved_memory", ContextProvenance::RetrievedMemory),
            (
                "relationship_context",
                ContextProvenance::RelationshipContext,
            ),
            ("retrieved_knowledge", ContextProvenance::RetrievedKnowledge),
            ("system_policy", ContextProvenance::SystemPolicy),
            ("controller_guidance", ContextProvenance::ControllerGuidance),
            ("tool_result", ContextProvenance::ToolResult),
        ] {
            let decoded: ContextProvenance =
                serde_json::from_str(&format!("\"{encoded}\"")).unwrap();
            assert_eq!(decoded, expected);
            assert_ne!(decoded, ContextProvenance::UserIngress);
        }
    }

    #[test]
    fn cancellation_and_sink_stop_are_explicit() {
        let cancellation = CancellationToken::default();
        let mut sink = |_event| Ok(StreamControl::Cancel);
        assert!(matches!(
            emit(
                &mut sink,
                &cancellation,
                ModelEvent::Finished {
                    reason: StopReason::EndTurn
                }
            ),
            Err(ProviderError {
                kind: ProviderErrorKind::Cancelled,
                ..
            })
        ));
        assert!(cancellation.is_cancelled());
    }

    #[test]
    fn parent_cancellation_propagates_without_child_cancellation_leaking_upward() {
        let client = CancellationToken::default();
        let goal = client.child_token();
        let session = goal.child_token();
        let provider = session.child_token();
        let tool = session.child_token();
        let kernel = session.child_token();
        let child = session.child_token();

        tool.cancel();
        assert!(tool.is_cancelled());
        assert!(!session.is_cancelled());
        assert!(!provider.is_cancelled());
        assert!(!kernel.is_cancelled());
        assert!(!child.is_cancelled());

        goal.cancel();
        for descendant in [&session, &provider, &kernel, &child] {
            assert!(descendant.is_cancelled());
        }
        assert!(!client.is_cancelled());
    }

    #[test]
    fn error_classes_only_fallback_for_transient_failures() {
        for kind in [
            ProviderErrorKind::Authentication,
            ProviderErrorKind::InvalidRequest,
            ProviderErrorKind::ContextOverflow,
            ProviderErrorKind::ContentRejected,
            ProviderErrorKind::Cancelled,
            ProviderErrorKind::MalformedResponse,
            ProviderErrorKind::Internal,
        ] {
            assert!(!ProviderError::new(kind, "terminal").allows_retry_or_fallback());
        }
        for kind in [
            ProviderErrorKind::RateLimited,
            ProviderErrorKind::Timeout,
            ProviderErrorKind::Unavailable,
        ] {
            assert!(ProviderError::new(kind, "transient").allows_retry_or_fallback());
        }
    }

    #[test]
    fn normalized_request_validates_and_counts() {
        let request = request();
        validate_request(&request).unwrap();
        assert!(approximate_token_count(&request).unwrap() > 0);
    }
}
