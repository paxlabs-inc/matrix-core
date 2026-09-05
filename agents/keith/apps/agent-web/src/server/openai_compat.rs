use std::collections::{BTreeMap, BTreeSet};
use std::convert::Infallible;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use axum::Json;
use axum::body::Bytes;
use axum::extract::rejection::BytesRejection;
use axum::extract::{Path, State};
use axum::http::{HeaderMap, HeaderValue, StatusCode, header};
use axum::response::sse::{Event, KeepAlive, Sse};
use axum::response::{IntoResponse, Response};
use futures_util::stream;
use keith_agent_types::{EntityId, ErrorCode, MessageId, ProfileId, SessionId, UtcTimestamp};
use keith_protocol::{
    AgentActivityKind, ClientCommand, CommandResult, CreateSession, DaemonEvent, DeliveryPolicy,
    MessageRole, ProfileSummary, ResponsePayload, SessionSnapshot, SubmitPrompt, UsageProjection,
    WireMessage,
};
use ring::{digest, hmac};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tokio::sync::{OwnedSemaphorePermit, Semaphore, mpsc};

use super::{AppState, BridgeError, DaemonBridge, NativeClient};

const AUTH_COMPARISON_KEY: &[u8] = b"keith-openai-compatibility-auth-v1";
const MODEL_PREFIX: &str = "keith:";
const BINDING_TITLE_PREFIX: &str = "keith-openai:";
const SESSION_HEADER: &str = "x-keith-session-id";
const CONVERSATION_HEADERS: [&str; 3] = [
    "x-keith-conversation-id",
    "x-openwebui-chat-id",
    "x-thread-id",
];
const CONVERSATION_METADATA: [&str; 3] = ["chat_id", "conversation_id", "thread_id"];
const MAX_ADVISORY_CLIENT_TOOLS: usize = 64;
const MAX_CLIENT_TOOL_NAME_BYTES: usize = 128;
const MAX_CLIENT_TOOL_DESCRIPTION_BYTES: usize = 4 * 1024;
const MAX_CLIENT_TOOL_SCHEMA_BYTES: usize = 64 * 1024;

pub struct OpenAiCompatibilityConfig {
    pub api_key: Vec<u8>,
    pub allow_non_loopback: bool,
    pub max_in_flight: usize,
}

impl std::fmt::Debug for OpenAiCompatibilityConfig {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("OpenAiCompatibilityConfig")
            .field("api_key", &"[REDACTED]")
            .field("allow_non_loopback", &self.allow_non_loopback)
            .field("max_in_flight", &self.max_in_flight)
            .finish()
    }
}

pub(super) struct OpenAiCompatibility {
    api_key_tag: [u8; 32],
    capacity: Arc<Semaphore>,
    session_resolution: Mutex<()>,
}

impl OpenAiCompatibility {
    pub(super) fn new(mut config: OpenAiCompatibilityConfig) -> Result<Self, String> {
        if config.api_key.len() < 32 {
            config.api_key.fill(0);
            return Err("OpenAI compatibility API key must contain at least 32 bytes".to_owned());
        }
        if config.max_in_flight == 0 {
            config.api_key.fill(0);
            return Err("OpenAI compatibility concurrency must be non-zero".to_owned());
        }
        let tag = hmac::sign(&comparison_key(), &config.api_key);
        let mut api_key_tag = [0_u8; 32];
        api_key_tag.copy_from_slice(tag.as_ref());
        config.api_key.fill(0);
        Ok(Self {
            api_key_tag,
            capacity: Arc::new(Semaphore::new(config.max_in_flight)),
            session_resolution: Mutex::new(()),
        })
    }

    fn authorize(&self, headers: &HeaderMap) -> Result<(), ApiFailure> {
        let supplied = bearer_token(headers).ok_or_else(ApiFailure::authentication)?;
        hmac::verify(&comparison_key(), supplied, &self.api_key_tag)
            .map_err(|_| ApiFailure::authentication())
    }

    fn admit(&self) -> Result<OwnedSemaphorePermit, ApiFailure> {
        Arc::clone(&self.capacity).try_acquire_owned().map_err(|_| {
            ApiFailure::new(
                StatusCode::TOO_MANY_REQUESTS,
                "rate_limit_error",
                "keith_capacity_exhausted",
                "Keith Agent compatibility capacity is exhausted",
                None,
            )
        })
    }
}

fn comparison_key() -> hmac::Key {
    hmac::Key::new(hmac::HMAC_SHA256, AUTH_COMPARISON_KEY)
}

fn bearer_token(headers: &HeaderMap) -> Option<&[u8]> {
    let value = headers.get(header::AUTHORIZATION)?.to_str().ok()?;
    let (scheme, token) = value.split_once(' ')?;
    (scheme.eq_ignore_ascii_case("bearer")
        && !token.is_empty()
        && !token.contains(char::is_whitespace))
    .then_some(token.as_bytes())
}

pub(super) async fn models(State(state): State<AppState>, headers: HeaderMap) -> Response {
    let compatibility = match configured(&state, &headers) {
        Ok(compatibility) => compatibility,
        Err(error) => return error.into_response(),
    };
    let permit = match compatibility.admit() {
        Ok(permit) => permit,
        Err(error) => return error.into_response(),
    };
    let bridge = state.bridge.clone();
    let result = tokio::task::spawn_blocking(move || {
        let _permit = permit;
        let mut client = bridge.connect().map_err(ApiFailure::bridge)?;
        let profiles = client.profiles().map_err(ApiFailure::bridge)?;
        Ok::<_, ApiFailure>(profiles)
    })
    .await;
    match result {
        Ok(Ok(profiles)) => openai_json(
            StatusCode::OK,
            &json!({
                "object": "list",
                "data": profiles
                    .iter()
                    .filter(|profile| profile.enabled)
                    .map(model_object)
                    .collect::<Vec<_>>()
            }),
            None,
        ),
        Ok(Err(error)) => error.into_response(),
        Err(_) => ApiFailure::task().into_response(),
    }
}

pub(super) async fn model(
    State(state): State<AppState>,
    Path(model): Path<String>,
    headers: HeaderMap,
) -> Response {
    let compatibility = match configured(&state, &headers) {
        Ok(compatibility) => compatibility,
        Err(error) => return error.into_response(),
    };
    let permit = match compatibility.admit() {
        Ok(permit) => permit,
        Err(error) => return error.into_response(),
    };
    let bridge = state.bridge.clone();
    let result = tokio::task::spawn_blocking(move || {
        let _permit = permit;
        let mut client = bridge.connect().map_err(ApiFailure::bridge)?;
        let profiles = client.profiles().map_err(ApiFailure::bridge)?;
        resolve_profile(&model, &profiles).cloned()
    })
    .await;
    match result {
        Ok(Ok(profile)) => openai_json(StatusCode::OK, &model_object(&profile), None),
        Ok(Err(error)) => error.into_response(),
        Err(_) => ApiFailure::task().into_response(),
    }
}

pub(super) async fn chat_completions(
    State(state): State<AppState>,
    headers: HeaderMap,
    body: Result<Bytes, BytesRejection>,
) -> Response {
    let compatibility = match configured(&state, &headers) {
        Ok(compatibility) => compatibility,
        Err(error) => return error.into_response(),
    };
    let Ok(body) = body else {
        return ApiFailure::payload_too_large().into_response();
    };
    let request: ChatCompletionRequest = match serde_json::from_slice(&body) {
        Ok(request) => request,
        Err(_) => {
            return ApiFailure::invalid(
                "request body must be valid OpenAI Chat Completions JSON",
                None,
            )
            .into_response();
        }
    };
    let prepared = match PreparedRequest::new(request, &headers) {
        Ok(prepared) => prepared,
        Err(error) => return error.into_response(),
    };
    let permit = match compatibility.admit() {
        Ok(permit) => permit,
        Err(error) => return error.into_response(),
    };
    if prepared.stream {
        stream_completion(state.bridge, compatibility, prepared, permit)
    } else {
        complete_once(state.bridge, compatibility, prepared, permit).await
    }
}

fn configured(
    state: &AppState,
    headers: &HeaderMap,
) -> Result<Arc<OpenAiCompatibility>, ApiFailure> {
    let compatibility = state.openai_compatibility.clone().ok_or_else(|| {
        ApiFailure::new(
            StatusCode::NOT_FOUND,
            "invalid_request_error",
            "compatibility_api_disabled",
            "OpenAI compatibility is not enabled",
            None,
        )
    })?;
    compatibility.authorize(headers)?;
    Ok(compatibility)
}

async fn complete_once(
    bridge: DaemonBridge,
    compatibility: Arc<OpenAiCompatibility>,
    prepared: PreparedRequest,
    permit: OwnedSemaphorePermit,
) -> Response {
    let completion_id = format!("chatcmpl-{}", EntityId::new());
    let created = unix_seconds();
    let result = tokio::task::spawn_blocking(move || {
        let _permit = permit;
        run_native_turn(&bridge, &compatibility, &prepared, completion_id, created)
    })
    .await;
    match result {
        Ok(Ok(completion)) => completion.into_response(),
        Ok(Err(error)) => error.into_response(),
        Err(_) => ApiFailure::task().into_response(),
    }
}

#[allow(clippy::too_many_lines)]
fn stream_completion(
    bridge: DaemonBridge,
    compatibility: Arc<OpenAiCompatibility>,
    prepared: PreparedRequest,
    permit: OwnedSemaphorePermit,
) -> Response {
    let completion_id = format!("chatcmpl-{}", EntityId::new());
    let created = unix_seconds();
    let requested_model = prepared.model.clone();
    let include_usage = prepared.include_stream_usage;
    let (sender, receiver) = mpsc::channel::<Result<Event, Infallible>>(256);
    tokio::spawn(async move {
        send_sse_json(
            &sender,
            &stream_chunk(
                &completion_id,
                created,
                &requested_model,
                &json!({"role": "assistant", "content": ""}),
                &Value::Null,
                None,
            ),
        )
        .await;
        let event_sender = sender.clone();
        let turn = tokio::task::spawn_blocking(move || {
            let _permit = permit;
            let mut projection = OpenAiStreamProjection::new(
                completion_id.clone(),
                created,
                requested_model.clone(),
            );
            let completion = run_native_turn_streaming(
                &bridge,
                &compatibility,
                &prepared,
                completion_id,
                created,
                &mut |message| {
                    for value in projection.project(message) {
                        send_sse_json_blocking(&event_sender, &value);
                    }
                },
            );
            (completion, projection.last_message)
        })
        .await;
        match turn {
            Ok((Ok(completion), last_message)) => {
                let id = completion.id.clone();
                let model = completion.model.clone();
                let session_id = completion.session_id.to_string();
                if last_message != completion.text {
                    send_sse_json(
                        &sender,
                        &stream_chunk(
                            &id,
                            completion.created,
                            &model,
                            &json!({"content": completion.text}),
                            &Value::Null,
                            Some(&session_id),
                        ),
                    )
                    .await;
                }
                send_sse_json(
                    &sender,
                    &stream_chunk(
                        &id,
                        completion.created,
                        &model,
                        &json!({}),
                        &json!("stop"),
                        Some(&session_id),
                    ),
                )
                .await;
                if include_usage {
                    send_sse_json(
                        &sender,
                        &json!({
                            "id": id,
                            "object": "chat.completion.chunk",
                            "created": completion.created,
                            "model": model,
                            "choices": [],
                            "usage": usage_json(completion.usage),
                            "metadata": {"keith_session_id": session_id}
                        }),
                    )
                    .await;
                }
            }
            Ok((Err(error), _)) => send_sse_json(&sender, &error.envelope()).await,
            Err(_) => send_sse_json(&sender, &ApiFailure::task().envelope()).await,
        }
        let _ = sender.send(Ok(Event::default().data("[DONE]"))).await;
    });
    let events = stream::unfold(receiver, |mut receiver| async move {
        receiver.recv().await.map(|event| (event, receiver))
    });
    let mut response = Sse::new(events)
        .keep_alive(
            KeepAlive::new()
                .interval(Duration::from_secs(15))
                .text("keep-alive"),
        )
        .into_response();
    response
        .headers_mut()
        .insert(header::CACHE_CONTROL, HeaderValue::from_static("no-store"));
    response.headers_mut().insert(
        header::X_CONTENT_TYPE_OPTIONS,
        HeaderValue::from_static("nosniff"),
    );
    response
}

async fn send_sse_json(sender: &mpsc::Sender<Result<Event, Infallible>>, value: &Value) {
    if let Ok(encoded) = serde_json::to_string(value) {
        let _ = sender.send(Ok(Event::default().data(encoded))).await;
    }
}

fn send_sse_json_blocking(sender: &mpsc::Sender<Result<Event, Infallible>>, value: &Value) {
    if let Ok(encoded) = serde_json::to_string(value) {
        let _ = sender.blocking_send(Ok(Event::default().data(encoded)));
    }
}

struct OpenAiStreamProjection {
    id: String,
    created: i64,
    model: String,
    last_message: String,
}

impl OpenAiStreamProjection {
    fn new(id: String, created: i64, model: String) -> Self {
        Self {
            id,
            created,
            model,
            last_message: String::new(),
        }
    }

    fn project(&mut self, message: WireMessage) -> Vec<Value> {
        match message {
            WireMessage::Event(envelope) => {
                if matches!(
                    &envelope.event,
                    DaemonEvent::AgentActivity(activity)
                        if matches!(&activity.kind, AgentActivityKind::AssistantStarted { .. })
                ) {
                    self.last_message.clear();
                }
                let delta_text = match &envelope.event {
                    DaemonEvent::AssistantDelta { text, .. } => Some(text.clone()),
                    _ => None,
                };
                let metadata = json!({
                    "keith_event": {
                        "type": "event",
                        "envelope": envelope
                    }
                });
                if let Some(text) = delta_text {
                    self.last_message.push_str(&text);
                    let mut chunk = stream_chunk(
                        &self.id,
                        self.created,
                        &self.model,
                        &json!({"content": text}),
                        &Value::Null,
                        None,
                    );
                    chunk["metadata"] = metadata;
                    vec![chunk]
                } else {
                    vec![activity_chunk(
                        &self.id,
                        self.created,
                        &self.model,
                        &metadata,
                    )]
                }
            }
            WireMessage::Snapshot(frame) => vec![activity_chunk(
                &self.id,
                self.created,
                &self.model,
                &json!({
                    "keith_event": {
                        "type": "snapshot",
                        "session_id": frame.snapshot.session.session_id,
                        "generation": frame.generation,
                        "sequence": frame.sequence,
                        "terminal": frame.snapshot.terminal
                    }
                }),
            )],
            WireMessage::Terminal(frame) => vec![activity_chunk(
                &self.id,
                self.created,
                &self.model,
                &json!({"keith_event": {"type": "terminal", "frame": frame}}),
            )],
            _ => Vec::new(),
        }
    }
}

fn activity_chunk(id: &str, created: i64, model: &str, metadata: &Value) -> Value {
    json!({
        "id": id,
        "object": "chat.completion.chunk",
        "created": created,
        "model": model,
        "choices": [],
        "usage": null,
        "metadata": metadata
    })
}

fn stream_chunk(
    id: &str,
    created: i64,
    model: &str,
    delta: &Value,
    finish_reason: &Value,
    session_id: Option<&str>,
) -> Value {
    let mut chunk = json!({
        "id": id,
        "object": "chat.completion.chunk",
        "created": created,
        "model": model,
        "choices": [{
            "index": 0,
            "delta": delta,
            "logprobs": null,
            "finish_reason": finish_reason
        }],
        "usage": null
    });
    if let Some(session_id) = session_id {
        chunk["metadata"] = json!({"keith_session_id": session_id});
    }
    chunk
}

#[derive(Debug, Deserialize)]
struct ChatCompletionRequest {
    model: String,
    messages: Vec<ChatMessage>,
    #[serde(default)]
    stream: bool,
    stream_options: Option<StreamOptions>,
    n: Option<u32>,
    tools: Option<Vec<Value>>,
    functions: Option<Vec<Value>>,
    tool_choice: Option<Value>,
    function_call: Option<Value>,
    modalities: Option<Vec<String>>,
    audio: Option<Value>,
    response_format: Option<ResponseFormat>,
    logprobs: Option<bool>,
    top_logprobs: Option<u8>,
    metadata: Option<BTreeMap<String, String>>,
    user: Option<String>,
}

#[derive(Debug, Deserialize)]
struct StreamOptions {
    #[serde(default)]
    include_usage: bool,
}

#[derive(Debug, Deserialize)]
struct ResponseFormat {
    #[serde(rename = "type")]
    kind: String,
}

#[derive(Debug, Deserialize)]
struct ChatMessage {
    role: String,
    content: Option<MessageContent>,
    name: Option<String>,
    tool_call_id: Option<String>,
    tool_calls: Option<Vec<Value>>,
    function_call: Option<Value>,
}

#[derive(Debug, Deserialize)]
#[serde(untagged)]
enum MessageContent {
    Text(String),
    Parts(Vec<ContentPart>),
}

#[derive(Debug, Deserialize)]
struct ContentPart {
    #[serde(rename = "type")]
    kind: String,
    text: Option<String>,
}

#[derive(Clone, Debug, Serialize)]
struct PreparedMessage {
    role: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    name: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    tool_call_id: Option<String>,
    content: String,
}

#[derive(Debug)]
struct PreparedRequest {
    model: String,
    messages: Vec<PreparedMessage>,
    advisory_client_tools: bool,
    stream: bool,
    include_stream_usage: bool,
    explicit_session: Option<SessionId>,
    conversation_binding: Option<String>,
    user: Option<String>,
}

impl PreparedRequest {
    #[allow(clippy::too_many_lines)]
    fn new(request: ChatCompletionRequest, headers: &HeaderMap) -> Result<Self, ApiFailure> {
        if request.model.trim().is_empty() {
            return Err(ApiFailure::invalid(
                "model must not be empty",
                Some("model"),
            ));
        }
        if request.messages.is_empty() {
            return Err(ApiFailure::invalid(
                "messages must contain at least one message",
                Some("messages"),
            ));
        }
        if request.n.unwrap_or(1) != 1 {
            return Err(ApiFailure::unsupported(
                "multiple completion choices are not supported",
                Some("n"),
            ));
        }
        let advisory_client_tools =
            validate_advisory_client_tools(request.tools.as_deref(), request.functions.as_deref())?;
        validate_advisory_tool_choice(request.tool_choice.as_ref(), "tool_choice")?;
        validate_advisory_tool_choice(request.function_call.as_ref(), "function_call")?;
        if request
            .modalities
            .as_ref()
            .is_some_and(|modalities| modalities.as_slice() != ["text"])
            || request.audio.is_some()
        {
            return Err(ApiFailure::unsupported(
                "only text input and output are supported",
                Some("modalities"),
            ));
        }
        if request
            .response_format
            .as_ref()
            .is_some_and(|format| format.kind != "text")
        {
            return Err(ApiFailure::unsupported(
                "only the text response format is supported",
                Some("response_format"),
            ));
        }
        if request.logprobs.unwrap_or(false) || request.top_logprobs.is_some() {
            return Err(ApiFailure::unsupported(
                "log probabilities are not exposed by the Keith agent runtime",
                Some("logprobs"),
            ));
        }
        let messages = request
            .messages
            .into_iter()
            .map(prepare_message)
            .collect::<Result<Vec<_>, _>>()?;
        let explicit_header = optional_header(headers, SESSION_HEADER)?;
        let explicit_metadata = request
            .metadata
            .as_ref()
            .and_then(|metadata| metadata.get("keith_session_id"))
            .cloned();
        let explicit = unique_value(
            [explicit_header, explicit_metadata].into_iter().flatten(),
            "native session",
        )?;
        let explicit_session = explicit
            .map(|session| {
                session
                    .parse()
                    .map_err(|_| ApiFailure::invalid("native session ID is invalid", None))
            })
            .transpose()?;
        let mut bindings = CONVERSATION_HEADERS
            .iter()
            .map(|name| optional_header(headers, name))
            .collect::<Result<Vec<_>, _>>()?;
        if let Some(metadata) = request.metadata.as_ref() {
            bindings.extend(
                CONVERSATION_METADATA
                    .iter()
                    .filter_map(|name| metadata.get(*name).cloned().map(Some)),
            );
        }
        let conversation_binding =
            unique_value(bindings.into_iter().flatten(), "conversation binding")?;
        if conversation_binding
            .as_ref()
            .is_some_and(|binding| binding.is_empty() || binding.len() > 512)
        {
            return Err(ApiFailure::invalid(
                "conversation binding must contain between 1 and 512 bytes",
                None,
            ));
        }
        if request
            .user
            .as_ref()
            .is_some_and(|user| user.is_empty() || user.len() > 512)
        {
            return Err(ApiFailure::invalid(
                "user must contain between 1 and 512 bytes",
                Some("user"),
            ));
        }
        Ok(Self {
            model: request.model,
            messages,
            advisory_client_tools,
            stream: request.stream,
            include_stream_usage: request
                .stream_options
                .is_some_and(|options| options.include_usage),
            explicit_session,
            conversation_binding,
            user: request.user,
        })
    }
}

fn validate_advisory_client_tools(
    tools: Option<&[Value]>,
    functions: Option<&[Value]>,
) -> Result<bool, ApiFailure> {
    let tool_count = tools.map_or(0, <[Value]>::len);
    let function_count = functions.map_or(0, <[Value]>::len);
    if tool_count.saturating_add(function_count) > MAX_ADVISORY_CLIENT_TOOLS {
        return Err(ApiFailure::invalid(
            "client function declaration count exceeds the compatibility limit",
            Some("tools"),
        ));
    }
    let mut names = BTreeSet::new();
    for tool in tools.into_iter().flatten() {
        let object = tool
            .as_object()
            .ok_or_else(|| ApiFailure::invalid("each tool must be an object", Some("tools")))?;
        if object.get("type").and_then(Value::as_str) != Some("function") {
            return Err(ApiFailure::unsupported(
                "only advisory function tool declarations are accepted",
                Some("tools"),
            ));
        }
        let function = object.get("function").ok_or_else(|| {
            ApiFailure::invalid("function tool is missing its definition", Some("tools"))
        })?;
        validate_advisory_function(function, "tools", &mut names)?;
    }
    for function in functions.into_iter().flatten() {
        validate_advisory_function(function, "functions", &mut names)?;
    }
    Ok(tool_count.saturating_add(function_count) > 0)
}

fn validate_advisory_function(
    value: &Value,
    parameter: &'static str,
    names: &mut BTreeSet<String>,
) -> Result<(), ApiFailure> {
    let function = value.as_object().ok_or_else(|| {
        ApiFailure::invalid("function definition must be an object", Some(parameter))
    })?;
    let name = function
        .get("name")
        .and_then(Value::as_str)
        .ok_or_else(|| {
            ApiFailure::invalid("function definition is missing its name", Some(parameter))
        })?;
    if name.is_empty()
        || name.len() > MAX_CLIENT_TOOL_NAME_BYTES
        || !name
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
    {
        return Err(ApiFailure::invalid(
            "function name is invalid or exceeds the compatibility limit",
            Some(parameter),
        ));
    }
    if !names.insert(name.to_owned()) {
        return Err(ApiFailure::invalid(
            "function names must be unique",
            Some(parameter),
        ));
    }
    if function.get("description").is_some_and(|description| {
        description
            .as_str()
            .is_none_or(|description| description.len() > MAX_CLIENT_TOOL_DESCRIPTION_BYTES)
    }) {
        return Err(ApiFailure::invalid(
            "function description is invalid or exceeds the compatibility limit",
            Some(parameter),
        ));
    }
    if function
        .get("strict")
        .is_some_and(|strict| !strict.is_boolean())
    {
        return Err(ApiFailure::invalid(
            "function strict flag must be a boolean",
            Some(parameter),
        ));
    }
    if let Some(parameters) = function.get("parameters")
        && (!parameters.is_object()
            || serde_json::to_vec(parameters)
                .map_err(|_| ApiFailure::protocol())?
                .len()
                > MAX_CLIENT_TOOL_SCHEMA_BYTES)
    {
        return Err(ApiFailure::invalid(
            "function parameter schema is invalid or exceeds the compatibility limit",
            Some(parameter),
        ));
    }
    Ok(())
}

fn validate_advisory_tool_choice(
    choice: Option<&Value>,
    parameter: &'static str,
) -> Result<(), ApiFailure> {
    let Some(choice) = choice else {
        return Ok(());
    };
    match choice.as_str() {
        Some("auto" | "none") => Ok(()),
        Some("required") => Err(ApiFailure::unsupported(
            "required client function execution is not supported; Keith's profile-owned tools remain active",
            Some(parameter),
        )),
        Some(_) => Err(ApiFailure::invalid(
            "client tool choice is invalid",
            Some(parameter),
        )),
        None if choice.is_object() => Err(ApiFailure::unsupported(
            "forced client function execution is not supported; Keith's profile-owned tools remain active",
            Some(parameter),
        )),
        None => Err(ApiFailure::invalid(
            "client tool choice must be a string or function selection object",
            Some(parameter),
        )),
    }
}

fn prepare_message(message: ChatMessage) -> Result<PreparedMessage, ApiFailure> {
    if !matches!(
        message.role.as_str(),
        "system" | "developer" | "user" | "assistant" | "tool" | "function"
    ) {
        return Err(ApiFailure::invalid(
            "message role is not supported",
            Some("messages.role"),
        ));
    }
    if message
        .tool_calls
        .as_ref()
        .is_some_and(|calls| !calls.is_empty())
        || message.function_call.is_some()
    {
        return Err(ApiFailure::unsupported(
            "client-defined tool-call history is not supported",
            Some("messages.tool_calls"),
        ));
    }
    let content = match message.content {
        Some(MessageContent::Text(text)) => text,
        Some(MessageContent::Parts(parts)) => {
            let mut text = String::new();
            for part in parts {
                if part.kind != "text" {
                    return Err(ApiFailure::unsupported(
                        "only text message content parts are supported",
                        Some("messages.content"),
                    ));
                }
                let part = part.text.ok_or_else(|| {
                    ApiFailure::invalid(
                        "text content part is missing text",
                        Some("messages.content"),
                    )
                })?;
                text.push_str(&part);
            }
            text
        }
        None => {
            return Err(ApiFailure::invalid(
                "message content must contain text",
                Some("messages.content"),
            ));
        }
    };
    Ok(PreparedMessage {
        role: message.role,
        name: message.name,
        tool_call_id: message.tool_call_id,
        content,
    })
}

fn optional_header(headers: &HeaderMap, name: &str) -> Result<Option<String>, ApiFailure> {
    headers
        .get(name)
        .map(|value| {
            value
                .to_str()
                .ok()
                .filter(|value| !value.is_empty() && value.len() <= 512)
                .map(str::to_owned)
                .ok_or_else(|| ApiFailure::invalid("compatibility header is invalid", None))
        })
        .transpose()
}

fn unique_value(
    values: impl Iterator<Item = String>,
    name: &str,
) -> Result<Option<String>, ApiFailure> {
    let values = values.collect::<BTreeSet<_>>();
    if values.len() > 1 {
        return Err(ApiFailure::invalid(
            &format!("conflicting {name} values were supplied"),
            None,
        ));
    }
    Ok(values.into_iter().next())
}

struct NativeCompletion {
    id: String,
    created: i64,
    model: String,
    session_id: SessionId,
    message_id: MessageId,
    text: String,
    usage: UsageProjection,
}

impl NativeCompletion {
    fn into_response(self) -> Response {
        let session = self.session_id.to_string();
        openai_json(
            StatusCode::OK,
            &json!({
                "id": self.id,
                "object": "chat.completion",
                "created": self.created,
                "model": self.model,
                "choices": [{
                    "index": 0,
                    "message": {
                        "role": "assistant",
                        "content": self.text,
                        "refusal": null,
                        "annotations": []
                    },
                    "logprobs": null,
                    "finish_reason": "stop"
                }],
                "usage": usage_json(self.usage),
                "metadata": {
                    "keith_session_id": session,
                    "keith_message_id": self.message_id.to_string()
                },
                "system_fingerprint": format!("keith-agent-{}", env!("CARGO_PKG_VERSION"))
            }),
            Some(&self.session_id),
        )
    }
}

fn run_native_turn(
    bridge: &DaemonBridge,
    compatibility: &OpenAiCompatibility,
    prepared: &PreparedRequest,
    completion_id: String,
    created: i64,
) -> Result<NativeCompletion, ApiFailure> {
    run_native_turn_streaming(
        bridge,
        compatibility,
        prepared,
        completion_id,
        created,
        &mut |_| {},
    )
}

fn run_native_turn_streaming(
    bridge: &DaemonBridge,
    compatibility: &OpenAiCompatibility,
    prepared: &PreparedRequest,
    completion_id: String,
    created: i64,
    events: &mut dyn FnMut(WireMessage),
) -> Result<NativeCompletion, ApiFailure> {
    let mut client = bridge.connect().map_err(ApiFailure::bridge)?;
    let profiles = client.profiles().map_err(ApiFailure::bridge)?;
    let profile = resolve_profile(&prepared.model, &profiles)?.clone();
    let (before, continuing) = resolve_session(&mut client, compatibility, &profile, prepared)?;
    let before_ids = before
        .messages
        .iter()
        .map(|message| message.message_id.clone())
        .collect::<BTreeSet<_>>();
    let prompt = render_prompt(
        &prepared.messages,
        continuing,
        prepared.advisory_client_tools,
    )?;
    let session_id = before.session.session_id.clone();
    let after = execute_snapshot_streaming(
        &mut client,
        Some(session_id.clone()),
        ClientCommand::SubmitPrompt(SubmitPrompt {
            session_id: session_id.clone(),
            text: prompt,
            artifacts: Vec::new(),
            delivery: DeliveryPolicy::Immediate,
            reply_route: None,
        }),
        events,
    )?;
    let message = after
        .messages
        .iter()
        .rev()
        .find(|message| {
            message.role == MessageRole::Assistant
                && message.committed
                && !before_ids.contains(&message.message_id)
        })
        .ok_or_else(|| {
            ApiFailure::new(
                StatusCode::CONFLICT,
                "api_error",
                "keith_turn_incomplete",
                "Keith Agent did not commit a final assistant message",
                None,
            )
        })?;
    Ok(NativeCompletion {
        id: completion_id,
        created,
        model: model_id(&profile),
        session_id,
        message_id: message.message_id.clone(),
        text: message.text.clone(),
        usage: usage_delta(before.usage, after.usage),
    })
}

fn resolve_session(
    client: &mut NativeClient,
    compatibility: &OpenAiCompatibility,
    profile: &ProfileSummary,
    prepared: &PreparedRequest,
) -> Result<(SessionSnapshot, bool), ApiFailure> {
    if let Some(session_id) = prepared.explicit_session.as_ref() {
        let belongs = client
            .sessions(Some(profile.id.clone()))
            .map_err(ApiFailure::bridge)?
            .into_iter()
            .any(|session| &session.session_id == session_id);
        if !belongs {
            return Err(ApiFailure::new(
                StatusCode::FORBIDDEN,
                "invalid_request_error",
                "keith_session_scope_mismatch",
                "native session does not belong to the selected Keith profile",
                Some(SESSION_HEADER),
            ));
        }
        return execute_snapshot(
            client,
            Some(session_id.clone()),
            ClientCommand::ResumeSession {
                session_id: session_id.clone(),
            },
        )
        .map(|snapshot| (snapshot, true));
    }
    let title = prepared
        .conversation_binding
        .as_ref()
        .map(|binding| binding_title(&profile.id, binding, prepared.user.as_deref()));
    if let Some(title) = title {
        let _resolution = compatibility.session_resolution.lock().map_err(|_| {
            ApiFailure::new(
                StatusCode::SERVICE_UNAVAILABLE,
                "api_error",
                "keith_session_resolution_unavailable",
                "durable conversation binding is temporarily unavailable",
                None,
            )
        })?;
        let sessions = client
            .sessions(Some(profile.id.clone()))
            .map_err(ApiFailure::bridge)?;
        if let Some(session) = sessions
            .iter()
            .filter(|session| session.title.as_deref() == Some(title.as_str()))
            .max_by_key(|session| session.updated_at)
        {
            return execute_snapshot(
                client,
                Some(session.session_id.clone()),
                ClientCommand::ResumeSession {
                    session_id: session.session_id.clone(),
                },
            )
            .map(|snapshot| (snapshot, true));
        }
        return create_session(client, profile, Some(title)).map(|snapshot| (snapshot, false));
    }
    create_session(
        client,
        profile,
        Some(format!("OpenAI compatibility {}", EntityId::new())),
    )
    .map(|snapshot| (snapshot, false))
}

fn create_session(
    client: &mut NativeClient,
    profile: &ProfileSummary,
    title: Option<String>,
) -> Result<SessionSnapshot, ApiFailure> {
    execute_snapshot(
        client,
        None,
        ClientCommand::CreateSession(CreateSession {
            profile_id: profile.id.clone(),
            workspace_id: profile.workspace_id.clone(),
            title,
        }),
    )
}

fn execute_snapshot(
    client: &mut NativeClient,
    session_id: Option<SessionId>,
    command: ClientCommand,
) -> Result<SessionSnapshot, ApiFailure> {
    execute_snapshot_streaming(client, session_id, command, &mut |_| {})
}

fn execute_snapshot_streaming(
    client: &mut NativeClient,
    session_id: Option<SessionId>,
    command: ClientCommand,
    events: &mut dyn FnMut(WireMessage),
) -> Result<SessionSnapshot, ApiFailure> {
    let envelope = client.envelope(session_id, command);
    let result = client
        .execute_streaming(envelope, events)
        .map_err(ApiFailure::bridge)?;
    match result.result {
        CommandResult::Data(payload) => match *payload {
            ResponsePayload::Snapshot(snapshot) => Ok(*snapshot),
            _ => Err(ApiFailure::protocol()),
        },
        CommandResult::Rejected(error) => Err(ApiFailure::native(error.error)),
        CommandResult::Accepted { .. } => Err(ApiFailure::protocol()),
    }
}

fn resolve_profile<'a>(
    requested: &str,
    profiles: &'a [ProfileSummary],
) -> Result<&'a ProfileSummary, ApiFailure> {
    let enabled = profiles
        .iter()
        .filter(|profile| profile.enabled)
        .collect::<Vec<_>>();
    if requested == "keith" {
        return match enabled.as_slice() {
            [profile] => Ok(*profile),
            [] => Err(ApiFailure::model_not_found()),
            _ => Err(ApiFailure::invalid(
                "model alias `keith` is ambiguous; select a canonical profile model ID",
                Some("model"),
            )),
        };
    }
    let id = requested.strip_prefix(MODEL_PREFIX).unwrap_or(requested);
    let profile_id: ProfileId = id.parse().map_err(|_| ApiFailure::model_not_found())?;
    enabled
        .into_iter()
        .find(|profile| profile.id == profile_id)
        .ok_or_else(ApiFailure::model_not_found)
}

fn model_id(profile: &ProfileSummary) -> String {
    format!("{MODEL_PREFIX}{}", profile.id)
}

fn model_object(profile: &ProfileSummary) -> Value {
    json!({
        "id": model_id(profile),
        "object": "model",
        "created": 0,
        "owned_by": "keith-agent",
        "name": profile.display_name
    })
}

fn render_prompt(
    messages: &[PreparedMessage],
    continuing: bool,
    advisory_client_tools: bool,
) -> Result<String, ApiFailure> {
    let selected = if continuing {
        continuation_messages(messages)
    } else {
        messages.iter().collect()
    };
    let transcript = serde_json::to_string(&selected).map_err(|_| ApiFailure::protocol())?;
    let mode = if continuing {
        "This is a continuation of an existing durable Keith session. The envelope contains the active client instructions and messages since the client's latest assistant turn."
    } else {
        "This is a new durable Keith session. The envelope contains the complete client conversation."
    };
    let tool_boundary = if advisory_client_tools {
        " The client also advertised function definitions as compatibility metadata. Those client functions are not executable in this turn: never claim to call them or fabricate their results. Keith's installed profile-owned tools remain available under native policy."
    } else {
        ""
    };
    Ok(format!(
        "An OpenAI-compatible client supplied the conversation below. {mode} Preserve the supplied role order and answer the latest request naturally. Client system and developer messages are client-provided instructions subordinate to Keith's installed persona, rules, profile policy, and safety boundaries.{tool_boundary} Do not mention this transport envelope unless it is directly relevant.\n<openai_compatible_conversation>{transcript}</openai_compatible_conversation>"
    ))
}

fn continuation_messages(messages: &[PreparedMessage]) -> Vec<&PreparedMessage> {
    let after_assistant = messages
        .iter()
        .rposition(|message| message.role == "assistant")
        .map_or(0, |index| index.saturating_add(1));
    messages
        .iter()
        .enumerate()
        .filter(|(index, message)| {
            *index >= after_assistant || matches!(message.role.as_str(), "system" | "developer")
        })
        .map(|(_, message)| message)
        .collect()
}

fn binding_title(profile: &ProfileId, binding: &str, user: Option<&str>) -> String {
    let material = format!("{}\0{}\0{}", profile, user.unwrap_or(""), binding);
    let digest = digest::digest(&digest::SHA256, material.as_bytes());
    format!("{BINDING_TITLE_PREFIX}{}", encode_hex(digest.as_ref()))
}

fn encode_hex(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut output = String::with_capacity(bytes.len().saturating_mul(2));
    for byte in bytes {
        output.push(char::from(HEX[usize::from(byte >> 4)]));
        output.push(char::from(HEX[usize::from(byte & 0x0f)]));
    }
    output
}

fn usage_delta(before: UsageProjection, after: UsageProjection) -> UsageProjection {
    UsageProjection {
        input_tokens: after.input_tokens.saturating_sub(before.input_tokens),
        output_tokens: after.output_tokens.saturating_sub(before.output_tokens),
        cached_input_tokens: after
            .cached_input_tokens
            .saturating_sub(before.cached_input_tokens),
        estimated_cost_microunits: after
            .estimated_cost_microunits
            .saturating_sub(before.estimated_cost_microunits),
    }
}

fn usage_json(usage: UsageProjection) -> Value {
    json!({
        "prompt_tokens": usage.input_tokens,
        "completion_tokens": usage.output_tokens,
        "total_tokens": usage.input_tokens.saturating_add(usage.output_tokens),
        "prompt_tokens_details": {
            "cached_tokens": usage.cached_input_tokens
        },
        "completion_tokens_details": {
            "reasoning_tokens": 0,
            "accepted_prediction_tokens": 0,
            "rejected_prediction_tokens": 0
        }
    })
}

fn unix_seconds() -> i64 {
    UtcTimestamp::now()
        .unwrap_or(UtcTimestamp::UNIX_EPOCH)
        .unix_millis()
        .div_euclid(1_000)
}

fn openai_json(status: StatusCode, value: &Value, session: Option<&SessionId>) -> Response {
    let mut response = (status, Json(value.clone())).into_response();
    response
        .headers_mut()
        .insert(header::CACHE_CONTROL, HeaderValue::from_static("no-store"));
    response.headers_mut().insert(
        header::X_CONTENT_TYPE_OPTIONS,
        HeaderValue::from_static("nosniff"),
    );
    if let Some(session) = session
        && let Ok(value) = HeaderValue::from_str(&session.to_string())
    {
        response.headers_mut().insert(SESSION_HEADER, value);
    }
    response
}

#[derive(Clone, Debug)]
struct ApiFailure {
    status: StatusCode,
    error_type: &'static str,
    code: &'static str,
    message: String,
    param: Option<&'static str>,
}

impl ApiFailure {
    fn new(
        status: StatusCode,
        error_type: &'static str,
        code: &'static str,
        message: impl Into<String>,
        param: Option<&'static str>,
    ) -> Self {
        Self {
            status,
            error_type,
            code,
            message: message.into(),
            param,
        }
    }

    fn authentication() -> Self {
        Self::new(
            StatusCode::UNAUTHORIZED,
            "authentication_error",
            "invalid_api_key",
            "invalid or missing bearer credential",
            None,
        )
    }

    fn invalid(message: &str, param: Option<&'static str>) -> Self {
        Self::new(
            StatusCode::BAD_REQUEST,
            "invalid_request_error",
            "invalid_request",
            message,
            param,
        )
    }

    fn unsupported(message: &str, param: Option<&'static str>) -> Self {
        Self::new(
            StatusCode::BAD_REQUEST,
            "invalid_request_error",
            "unsupported_feature",
            message,
            param,
        )
    }

    fn model_not_found() -> Self {
        Self::new(
            StatusCode::NOT_FOUND,
            "invalid_request_error",
            "model_not_found",
            "the requested Keith profile model does not exist or is disabled",
            Some("model"),
        )
    }

    fn payload_too_large() -> Self {
        Self::new(
            StatusCode::PAYLOAD_TOO_LARGE,
            "invalid_request_error",
            "request_too_large",
            "request body exceeds the configured compatibility limit",
            None,
        )
    }

    fn bridge(_error: BridgeError) -> Self {
        Self::new(
            StatusCode::SERVICE_UNAVAILABLE,
            "api_error",
            "keith_native_api_unavailable",
            "Keith Agent native API is unavailable",
            None,
        )
    }

    fn protocol() -> Self {
        Self::new(
            StatusCode::BAD_GATEWAY,
            "api_error",
            "keith_native_protocol_error",
            "Keith Agent native API returned an incompatible response",
            None,
        )
    }

    fn task() -> Self {
        Self::new(
            StatusCode::SERVICE_UNAVAILABLE,
            "api_error",
            "keith_adapter_unavailable",
            "OpenAI compatibility processing is unavailable",
            None,
        )
    }

    fn native(error: keith_agent_types::CommonError) -> Self {
        let status = match error.code {
            ErrorCode::InvalidInput => StatusCode::BAD_REQUEST,
            ErrorCode::NotFound => StatusCode::NOT_FOUND,
            ErrorCode::Conflict | ErrorCode::Cancelled => StatusCode::CONFLICT,
            ErrorCode::Unauthorized | ErrorCode::Forbidden => StatusCode::FORBIDDEN,
            ErrorCode::UnsupportedVersion => StatusCode::BAD_GATEWAY,
            ErrorCode::Unavailable => StatusCode::SERVICE_UNAVAILABLE,
            ErrorCode::ResourceExhausted => StatusCode::TOO_MANY_REQUESTS,
            ErrorCode::DeadlineExceeded => StatusCode::GATEWAY_TIMEOUT,
            ErrorCode::CorruptState | ErrorCode::Internal => StatusCode::INTERNAL_SERVER_ERROR,
        };
        Self::new(
            status,
            "api_error",
            "keith_native_error",
            error.message,
            None,
        )
    }

    fn envelope(&self) -> Value {
        json!({
            "error": {
                "message": self.message,
                "type": self.error_type,
                "param": self.param,
                "code": self.code
            }
        })
    }
}

impl IntoResponse for ApiFailure {
    fn into_response(self) -> Response {
        let mut response = openai_json(self.status, &self.envelope(), None);
        if self.status == StatusCode::UNAUTHORIZED {
            response.headers_mut().insert(
                header::WWW_AUTHENTICATE,
                HeaderValue::from_static("Bearer realm=\"keith-agent\""),
            );
        }
        response
    }
}

#[cfg(test)]
mod tests {
    use keith_agent_types::WorkspaceId;

    use super::*;

    fn profile() -> ProfileSummary {
        ProfileSummary {
            id: ProfileId::new(),
            workspace_id: WorkspaceId::new(),
            display_name: "Primary Keith".to_owned(),
            enabled: true,
        }
    }

    #[test]
    fn bearer_authentication_is_exact_and_redacted() {
        let compatibility = OpenAiCompatibility::new(OpenAiCompatibilityConfig {
            api_key: b"01234567890123456789012345678901".to_vec(),
            allow_non_loopback: false,
            max_in_flight: 1,
        })
        .unwrap();
        let mut headers = HeaderMap::new();
        headers.insert(
            header::AUTHORIZATION,
            HeaderValue::from_static("Bearer 01234567890123456789012345678901"),
        );
        assert!(compatibility.authorize(&headers).is_ok());
        headers.insert(
            header::AUTHORIZATION,
            HeaderValue::from_static("Bearer 01234567890123456789012345678902"),
        );
        assert!(compatibility.authorize(&headers).is_err());
    }

    #[test]
    fn profile_models_are_canonical_and_alias_is_unambiguous() {
        let first = profile();
        assert_eq!(
            resolve_profile("keith", std::slice::from_ref(&first)).unwrap(),
            &first
        );
        let canonical = model_id(&first);
        assert_eq!(
            resolve_profile(&canonical, std::slice::from_ref(&first)).unwrap(),
            &first
        );
        assert!(resolve_profile("missing", std::slice::from_ref(&first)).is_err());
        assert!(resolve_profile("keith", &[first, profile()]).is_err());
    }

    #[test]
    fn conversation_binding_is_private_and_profile_scoped() {
        let first = profile();
        let second = profile();
        let title = binding_title(&first.id, "private-chat-id", Some("private-user"));
        assert!(title.starts_with(BINDING_TITLE_PREFIX));
        assert!(!title.contains("private-chat-id"));
        assert!(!title.contains("private-user"));
        assert_ne!(
            title,
            binding_title(&second.id, "private-chat-id", Some("private-user"))
        );
    }

    #[test]
    fn ordered_text_roles_survive_and_continuation_drops_echoed_history() {
        let messages = vec![
            PreparedMessage {
                role: "system".to_owned(),
                name: None,
                tool_call_id: None,
                content: "client rules".to_owned(),
            },
            PreparedMessage {
                role: "user".to_owned(),
                name: None,
                tool_call_id: None,
                content: "first".to_owned(),
            },
            PreparedMessage {
                role: "assistant".to_owned(),
                name: None,
                tool_call_id: None,
                content: "answer".to_owned(),
            },
            PreparedMessage {
                role: "user".to_owned(),
                name: Some("owner".to_owned()),
                tool_call_id: None,
                content: "second".to_owned(),
            },
        ];
        let prompt = render_prompt(&messages, true, false).unwrap();
        assert!(prompt.contains("client rules"));
        assert!(prompt.contains("second"));
        assert!(!prompt.contains("first"));
        assert!(!prompt.contains("\"content\":\"answer\""));
        assert!(prompt.contains("\"role\":\"system\""));
        assert!(prompt.contains("\"role\":\"user\""));
    }

    #[test]
    fn advisory_client_tools_are_validated_without_receiving_runtime_authority() {
        let request = ChatCompletionRequest {
            model: "keith".to_owned(),
            messages: vec![ChatMessage {
                role: "user".to_owned(),
                content: Some(MessageContent::Text("hello".to_owned())),
                name: None,
                tool_call_id: None,
                tool_calls: None,
                function_call: None,
            }],
            stream: false,
            stream_options: None,
            n: None,
            tools: Some(vec![json!({
                "type": "function",
                "function": {
                    "name": "openwebui_search",
                    "description": "Search through the client UI",
                    "parameters": {
                        "type": "object",
                        "properties": {"query": {"type": "string"}},
                        "required": ["query"]
                    }
                }
            })]),
            functions: None,
            tool_choice: Some(json!("auto")),
            function_call: None,
            modalities: None,
            audio: None,
            response_format: None,
            logprobs: None,
            top_logprobs: None,
            metadata: None,
            user: None,
        };
        let prepared = PreparedRequest::new(request, &HeaderMap::new()).unwrap();
        assert!(prepared.advisory_client_tools);
        let prompt =
            render_prompt(&prepared.messages, false, prepared.advisory_client_tools).unwrap();
        assert!(prompt.contains("not executable"));
        assert!(!prompt.contains("openwebui_search"));
    }

    #[test]
    fn forced_client_tool_execution_fails_explicitly() {
        let request = ChatCompletionRequest {
            model: "keith".to_owned(),
            messages: vec![ChatMessage {
                role: "user".to_owned(),
                content: Some(MessageContent::Text("hello".to_owned())),
                name: None,
                tool_call_id: None,
                tool_calls: None,
                function_call: None,
            }],
            stream: false,
            stream_options: None,
            n: None,
            tools: Some(vec![json!({
                "type": "function",
                "function": {"name": "forced_tool", "parameters": {"type": "object"}}
            })]),
            functions: None,
            tool_choice: Some(json!("required")),
            function_call: None,
            modalities: None,
            audio: None,
            response_format: None,
            logprobs: None,
            top_logprobs: None,
            metadata: None,
            user: None,
        };
        let error = PreparedRequest::new(request, &HeaderMap::new()).unwrap_err();
        assert_eq!(error.code, "unsupported_feature");
        assert!(error.message.contains("required client function execution"));
    }

    #[test]
    fn stream_chunks_expose_the_resolved_native_session() {
        let session = SessionId::new().to_string();
        let chunk = stream_chunk(
            "chatcmpl-test",
            1,
            "keith:test",
            &json!({"content": "ready"}),
            &Value::Null,
            Some(&session),
        );
        assert_eq!(chunk["metadata"]["keith_session_id"], session);
    }
}
