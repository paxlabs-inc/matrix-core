#![forbid(unsafe_code)]

use std::collections::BTreeMap;
use std::fmt::{self, Debug, Display};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Weak};

use keith_agent_types::{EntityId, ToolCallId};
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
pub enum ToolBehavior {
    ReadOnly,
    StateChanging,
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
    pub model: String,
    pub system: Vec<ContentBlock>,
    pub messages: Vec<Message>,
    pub tools: Vec<ToolDefinition>,
    pub max_output_tokens: Option<u32>,
    pub temperature: Option<f32>,
    pub reasoning_effort: Option<String>,
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
        ModelRequest {
            request_id: EntityId::new(),
            model: "model-a".into(),
            system: Vec::new(),
            messages: vec![Message {
                role: MessageRole::User,
                content: vec![ContentBlock::Text {
                    text: "hello".into(),
                }],
            }],
            tools: Vec::new(),
            max_output_tokens: Some(100),
            temperature: None,
            reasoning_effort: None,
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
