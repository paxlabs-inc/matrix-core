#![forbid(unsafe_code)]

use std::collections::BTreeMap;
use std::collections::btree_map::Entry;
use std::io::{BufRead, BufReader};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Duration;

use keith_agent_types::{EntityId, ToolCallId};
use keith_provider_core::{
    CancellationToken, ContentBlock, Message, MessageRole, ModelDescriptor, ModelEvent,
    ModelEventSink, ModelProvider, ModelRequest, ProviderCredential, ProviderError,
    ProviderErrorKind, StopReason, ToolBehavior, Usage, approximate_token_count,
    classify_http_status, emit, validate_request,
};
use serde_json::{Value, json};
use ureq::http::Response;
use ureq::{Agent, Error as HttpError};

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ProviderHttpConfig {
    pub base_url: String,
    pub timeout: Duration,
    pub max_response_bytes: u64,
}

impl ProviderHttpConfig {
    /// # Errors
    ///
    /// Returns an invalid-request error for non-HTTPS non-loopback URLs or invalid limits.
    pub fn new(base_url: impl Into<String>) -> Result<Self, ProviderError> {
        let base_url = base_url.into().trim_end_matches('/').to_owned();
        let secure = base_url.starts_with("https://");
        let loopback = base_url.starts_with("http://127.0.0.1:")
            || base_url.starts_with("http://localhost:")
            || base_url.starts_with("http://[::1]:");
        if (!secure && !loopback) || base_url.len() > 2_048 {
            return Err(ProviderError::new(
                ProviderErrorKind::InvalidRequest,
                "provider base URL must use HTTPS or an explicit loopback address",
            ));
        }
        Ok(Self {
            base_url,
            timeout: Duration::from_secs(120),
            max_response_bytes: 32 * 1_024 * 1_024,
        })
    }
}

struct HttpRuntime {
    config: ProviderHttpConfig,
    agent: Agent,
    active: Mutex<BTreeMap<EntityId, CancellationToken>>,
}

impl HttpRuntime {
    fn new(config: ProviderHttpConfig) -> Result<Self, ProviderError> {
        if config.timeout.is_zero() || config.max_response_bytes == 0 {
            return Err(ProviderError::new(
                ProviderErrorKind::InvalidRequest,
                "provider timeout and response limit must be non-zero",
            ));
        }
        let https_only = config.base_url.starts_with("https://");
        let agent_config = Agent::config_builder()
            .timeout_global(Some(config.timeout))
            .https_only(https_only)
            .http_status_as_error(false)
            .build();
        Ok(Self {
            config,
            agent: Agent::new_with_config(agent_config),
            active: Mutex::new(BTreeMap::new()),
        })
    }

    fn register(
        &self,
        request_id: &EntityId,
        cancellation: &CancellationToken,
    ) -> Result<(), ProviderError> {
        let mut active = self.lock_active()?;
        if active
            .insert(request_id.clone(), cancellation.clone())
            .is_some()
        {
            return Err(ProviderError::new(
                ProviderErrorKind::InvalidRequest,
                "provider request ID is already active",
            ));
        }
        Ok(())
    }

    fn unregister(&self, request_id: &EntityId) {
        if let Ok(mut active) = self.active.lock() {
            active.remove(request_id);
        }
    }

    fn cancel(&self, request_id: &EntityId) -> Result<(), ProviderError> {
        let active = self.lock_active()?;
        let token = active.get(request_id).ok_or_else(|| {
            ProviderError::new(
                ProviderErrorKind::InvalidRequest,
                "provider request is not active",
            )
        })?;
        token.cancel();
        Ok(())
    }

    fn lock_active(
        &self,
    ) -> Result<MutexGuard<'_, BTreeMap<EntityId, CancellationToken>>, ProviderError> {
        self.active.lock().map_err(|_| {
            ProviderError::new(
                ProviderErrorKind::Internal,
                "provider cancellation registry lock was poisoned",
            )
        })
    }

    fn url(&self, path: &str) -> String {
        format!("{}{path}", self.config.base_url)
    }
}

#[derive(Clone)]
pub struct OpenAiProvider {
    runtime: Arc<HttpRuntime>,
    provider_id: String,
    chat_path: String,
    models_path: Option<String>,
    catalog_model: Option<String>,
    api_key_header: bool,
}

impl OpenAiProvider {
    /// # Errors
    ///
    /// Returns an error when the HTTP configuration is invalid.
    pub fn new(config: ProviderHttpConfig) -> Result<Self, ProviderError> {
        Ok(Self {
            runtime: Arc::new(HttpRuntime::new(config)?),
            provider_id: "openai".into(),
            chat_path: "/v1/chat/completions".into(),
            models_path: Some("/v1/models".into()),
            catalog_model: None,
            api_key_header: false,
        })
    }

    /// Creates an `OpenAI` chat-completions compatible provider whose base URL already
    /// contains any provider-specific API prefix such as `/v1` or `/api/v3`.
    ///
    /// # Errors
    ///
    /// Returns an error when the provider ID, model, or HTTP configuration is invalid.
    pub fn compatible(
        provider_id: impl Into<String>,
        config: ProviderHttpConfig,
        catalog_model: impl Into<String>,
        supports_model_discovery: bool,
    ) -> Result<Self, ProviderError> {
        let provider_id = provider_id.into();
        let catalog_model = catalog_model.into();
        if provider_id.trim().is_empty() || catalog_model.trim().is_empty() {
            return Err(ProviderError::new(
                ProviderErrorKind::InvalidRequest,
                "provider ID and catalog model must be non-empty",
            ));
        }
        Ok(Self {
            runtime: Arc::new(HttpRuntime::new(config)?),
            provider_id,
            chat_path: "/chat/completions".into(),
            models_path: supports_model_discovery.then(|| "/models".into()),
            catalog_model: Some(catalog_model),
            api_key_header: false,
        })
    }

    #[must_use]
    pub fn with_api_key_header(mut self) -> Self {
        self.api_key_header = true;
        self
    }

    fn stream_inner(
        &self,
        request: &ModelRequest,
        credential: &ProviderCredential,
        cancellation: &CancellationToken,
        sink: &mut dyn ModelEventSink,
    ) -> Result<Usage, ProviderError> {
        let uses_responses_api = self.provider_id == "openai"
            && request.model == "gpt-5.6-luna"
            && !request.tools.is_empty()
            && request.reasoning_effort.as_deref() != Some("none");
        let body = if uses_responses_api {
            openai_responses_request(request)?
        } else {
            openai_request(request)?
        };
        let path = if uses_responses_api {
            "/v1/responses"
        } else {
            &self.chat_path
        };
        let http_request = self
            .runtime
            .agent
            .post(self.runtime.url(path))
            .header("accept", "text/event-stream")
            .header("content-type", "application/json");
        let response = if self.api_key_header {
            http_request
                .header("api-key", credential.expose_utf8()?)
                .send(&body)
        } else {
            http_request
                .header(
                    "authorization",
                    &format!("Bearer {}", credential.expose_utf8()?),
                )
                .send(&body)
        }
        .map_err(map_http_error)?;
        let response = check_provider_response(response, self.runtime.config.max_response_bytes)?;
        if uses_responses_api {
            return parse_openai_responses_stream(
                response,
                request,
                cancellation,
                sink,
                self.runtime.config.max_response_bytes,
            );
        }
        emit(
            sink,
            cancellation,
            ModelEvent::Started {
                provider_request_id: request_header(&response, "x-request-id"),
                model: request.model.clone(),
            },
        )?;
        parse_openai_stream(
            response,
            request,
            cancellation,
            sink,
            self.runtime.config.max_response_bytes,
        )
    }
}

impl ModelProvider for OpenAiProvider {
    fn provider_id(&self) -> &str {
        &self.provider_id
    }

    fn list_models(
        &self,
        credential: &ProviderCredential,
    ) -> Result<Vec<ModelDescriptor>, ProviderError> {
        if let Some(path) = &self.models_path {
            let request = self.runtime.agent.get(self.runtime.url(path));
            let mut response = if self.api_key_header {
                request.header("api-key", credential.expose_utf8()?).call()
            } else {
                request
                    .header(
                        "authorization",
                        &format!("Bearer {}", credential.expose_utf8()?),
                    )
                    .call()
            }
            .map_err(map_http_error)?;
            check_status(response.status().as_u16())?;
            let bytes = read_bounded(&mut response, self.runtime.config.max_response_bytes)?;
            let value: Value = serde_json::from_slice(&bytes).map_err(malformed)?;
            model_list(&value, self.provider_id())
        } else {
            Ok(vec![catalog_descriptor(
                self.provider_id(),
                self.catalog_model.as_deref().ok_or_else(|| {
                    ProviderError::new(
                        ProviderErrorKind::Internal,
                        "compatible provider has no catalog model",
                    )
                })?,
            )])
        }
    }

    fn stream(
        &self,
        request: &ModelRequest,
        credential: &ProviderCredential,
        cancellation: &CancellationToken,
        sink: &mut dyn ModelEventSink,
    ) -> Result<Usage, ProviderError> {
        validate_request(request)?;
        cancellation.check()?;
        self.runtime.register(&request.request_id, cancellation)?;
        let result = self.stream_inner(request, credential, cancellation, sink);
        self.runtime.unregister(&request.request_id);
        result
    }

    fn count_tokens(
        &self,
        request: &ModelRequest,
        _credential: &ProviderCredential,
    ) -> Result<u64, ProviderError> {
        validate_request(request)?;
        approximate_token_count(request)
    }

    fn cancel(&self, request_id: &EntityId) -> Result<(), ProviderError> {
        self.runtime.cancel(request_id)
    }
}

#[derive(Clone)]
pub struct OpenAiResponsesProvider {
    runtime: Arc<HttpRuntime>,
    provider_id: String,
    responses_path: String,
    catalog_model: String,
    codex_account_header: bool,
}

impl OpenAiResponsesProvider {
    /// Creates the `ChatGPT` subscription Codex Responses adapter.
    ///
    /// # Errors
    ///
    /// Returns an error when the HTTP configuration is invalid.
    pub fn codex(
        config: ProviderHttpConfig,
        catalog_model: impl Into<String>,
    ) -> Result<Self, ProviderError> {
        Ok(Self {
            runtime: Arc::new(HttpRuntime::new(config)?),
            provider_id: "openai-codex".into(),
            responses_path: "/codex/responses".into(),
            catalog_model: catalog_model.into(),
            codex_account_header: true,
        })
    }

    fn stream_inner(
        &self,
        request: &ModelRequest,
        credential: &ProviderCredential,
        cancellation: &CancellationToken,
        sink: &mut dyn ModelEventSink,
    ) -> Result<Usage, ProviderError> {
        let body = openai_responses_request(request)?;
        let secret = credential.expose_utf8()?;
        let mut http_request = self
            .runtime
            .agent
            .post(self.runtime.url(&self.responses_path))
            .header("authorization", &format!("Bearer {secret}"))
            .header("openai-beta", "responses=experimental")
            .header("originator", "pi")
            .header("user-agent", "keith-agent/0.1")
            .header("accept", "text/event-stream")
            .header("content-type", "application/json");
        if self.codex_account_header {
            http_request = http_request.header("chatgpt-account-id", &codex_account_id(secret)?);
        }
        let response = http_request.send(&body).map_err(map_http_error)?;
        let response = check_provider_response(response, self.runtime.config.max_response_bytes)?;
        parse_openai_responses_stream(
            response,
            request,
            cancellation,
            sink,
            self.runtime.config.max_response_bytes,
        )
    }
}

impl ModelProvider for OpenAiResponsesProvider {
    fn provider_id(&self) -> &str {
        &self.provider_id
    }

    fn list_models(
        &self,
        _credential: &ProviderCredential,
    ) -> Result<Vec<ModelDescriptor>, ProviderError> {
        Ok(vec![catalog_descriptor(
            self.provider_id(),
            &self.catalog_model,
        )])
    }

    fn stream(
        &self,
        request: &ModelRequest,
        credential: &ProviderCredential,
        cancellation: &CancellationToken,
        sink: &mut dyn ModelEventSink,
    ) -> Result<Usage, ProviderError> {
        validate_request(request)?;
        cancellation.check()?;
        self.runtime.register(&request.request_id, cancellation)?;
        let result = self.stream_inner(request, credential, cancellation, sink);
        self.runtime.unregister(&request.request_id);
        result
    }

    fn count_tokens(
        &self,
        request: &ModelRequest,
        _credential: &ProviderCredential,
    ) -> Result<u64, ProviderError> {
        validate_request(request)?;
        approximate_token_count(request)
    }

    fn cancel(&self, request_id: &EntityId) -> Result<(), ProviderError> {
        self.runtime.cancel(request_id)
    }
}

#[derive(Clone)]
pub struct AmazonBedrockProvider {
    runtime: Arc<HttpRuntime>,
    catalog_model: String,
}

impl AmazonBedrockProvider {
    /// Creates a Bedrock Converse adapter using the scoped
    /// `AWS_BEARER_TOKEN_BEDROCK` credential flow.
    ///
    /// # Errors
    ///
    /// Returns an error when the HTTP configuration or model is invalid.
    pub fn new(
        config: ProviderHttpConfig,
        catalog_model: impl Into<String>,
    ) -> Result<Self, ProviderError> {
        let catalog_model = catalog_model.into();
        if catalog_model.trim().is_empty() {
            return Err(ProviderError::new(
                ProviderErrorKind::InvalidRequest,
                "Bedrock catalog model must be non-empty",
            ));
        }
        Ok(Self {
            runtime: Arc::new(HttpRuntime::new(config)?),
            catalog_model,
        })
    }

    fn stream_inner(
        &self,
        request: &ModelRequest,
        credential: &ProviderCredential,
        cancellation: &CancellationToken,
        sink: &mut dyn ModelEventSink,
    ) -> Result<Usage, ProviderError> {
        let model = percent_encode_path_segment(&request.model);
        let path = format!("/model/{model}/converse");
        let response = self
            .runtime
            .agent
            .post(self.runtime.url(&path))
            .header(
                "authorization",
                &format!("Bearer {}", credential.expose_utf8()?),
            )
            .header("content-type", "application/json")
            .header("accept", "application/json")
            .send(&bedrock_request(request)?)
            .map_err(map_http_error)?;
        let mut response =
            check_provider_response(response, self.runtime.config.max_response_bytes)?;
        emit(
            sink,
            cancellation,
            ModelEvent::Started {
                provider_request_id: request_header(&response, "x-amzn-requestid"),
                model: request.model.clone(),
            },
        )?;
        let bytes = read_bounded(&mut response, self.runtime.config.max_response_bytes)?;
        cancellation.check()?;
        let value: Value = serde_json::from_slice(&bytes).map_err(malformed)?;
        normalize_bedrock_response(&value, cancellation, sink)
    }
}

impl ModelProvider for AmazonBedrockProvider {
    fn provider_id(&self) -> &'static str {
        "amazon-bedrock"
    }

    fn list_models(
        &self,
        _credential: &ProviderCredential,
    ) -> Result<Vec<ModelDescriptor>, ProviderError> {
        Ok(vec![catalog_descriptor(
            self.provider_id(),
            &self.catalog_model,
        )])
    }

    fn stream(
        &self,
        request: &ModelRequest,
        credential: &ProviderCredential,
        cancellation: &CancellationToken,
        sink: &mut dyn ModelEventSink,
    ) -> Result<Usage, ProviderError> {
        validate_request(request)?;
        cancellation.check()?;
        self.runtime.register(&request.request_id, cancellation)?;
        let result = self.stream_inner(request, credential, cancellation, sink);
        self.runtime.unregister(&request.request_id);
        result
    }

    fn count_tokens(
        &self,
        request: &ModelRequest,
        _credential: &ProviderCredential,
    ) -> Result<u64, ProviderError> {
        validate_request(request)?;
        approximate_token_count(request)
    }

    fn cancel(&self, request_id: &EntityId) -> Result<(), ProviderError> {
        self.runtime.cancel(request_id)
    }
}

#[derive(Clone)]
pub struct AnthropicProvider {
    runtime: Arc<HttpRuntime>,
    api_version: String,
    provider_id: String,
    messages_path: String,
    models_path: Option<String>,
    catalog_model: Option<String>,
    credential_header: String,
    bearer_authentication: bool,
    default_headers: Vec<(String, String)>,
}

impl AnthropicProvider {
    /// # Errors
    ///
    /// Returns an error when the HTTP configuration is invalid.
    pub fn new(config: ProviderHttpConfig) -> Result<Self, ProviderError> {
        Ok(Self {
            runtime: Arc::new(HttpRuntime::new(config)?),
            api_version: "2023-06-01".into(),
            provider_id: "anthropic".into(),
            messages_path: "/v1/messages".into(),
            models_path: Some("/v1/models".into()),
            catalog_model: None,
            credential_header: "x-api-key".into(),
            bearer_authentication: false,
            default_headers: Vec::new(),
        })
    }

    /// Creates an Anthropic Messages compatible provider whose base URL contains
    /// any provider-specific prefix.
    ///
    /// # Errors
    ///
    /// Returns an error when the provider ID, model, or HTTP configuration is invalid.
    pub fn compatible(
        provider_id: impl Into<String>,
        config: ProviderHttpConfig,
        catalog_model: impl Into<String>,
        bearer_authentication: bool,
    ) -> Result<Self, ProviderError> {
        let provider_id = provider_id.into();
        let catalog_model = catalog_model.into();
        if provider_id.trim().is_empty() || catalog_model.trim().is_empty() {
            return Err(ProviderError::new(
                ProviderErrorKind::InvalidRequest,
                "provider ID and catalog model must be non-empty",
            ));
        }
        Ok(Self {
            runtime: Arc::new(HttpRuntime::new(config)?),
            api_version: "2023-06-01".into(),
            provider_id,
            messages_path: "/v1/messages".into(),
            models_path: None,
            catalog_model: Some(catalog_model),
            credential_header: if bearer_authentication {
                "authorization".into()
            } else {
                "x-api-key".into()
            },
            bearer_authentication,
            default_headers: Vec::new(),
        })
    }

    /// Overrides the header used to transmit this provider's scoped credential.
    ///
    /// # Errors
    ///
    /// Returns an error when the header name is empty.
    pub fn with_credential_header(
        mut self,
        header: impl Into<String>,
        bearer_authentication: bool,
    ) -> Result<Self, ProviderError> {
        let header = header.into();
        if header.trim().is_empty() {
            return Err(ProviderError::new(
                ProviderErrorKind::InvalidRequest,
                "credential header must be non-empty",
            ));
        }
        self.credential_header = header;
        self.bearer_authentication = bearer_authentication;
        Ok(self)
    }

    /// Adds a non-secret provider compatibility header.
    ///
    /// # Errors
    ///
    /// Returns an error when the name or value is empty.
    pub fn with_default_header(
        mut self,
        name: impl Into<String>,
        value: impl Into<String>,
    ) -> Result<Self, ProviderError> {
        let name = name.into();
        let value = value.into();
        if name.trim().is_empty() || value.trim().is_empty() {
            return Err(ProviderError::new(
                ProviderErrorKind::InvalidRequest,
                "provider compatibility header must be non-empty",
            ));
        }
        self.default_headers.push((name, value));
        Ok(self)
    }

    fn stream_inner(
        &self,
        request: &ModelRequest,
        credential: &ProviderCredential,
        cancellation: &CancellationToken,
        sink: &mut dyn ModelEventSink,
    ) -> Result<Usage, ProviderError> {
        let body = anthropic_request(request)?;
        let mut http_request = self
            .runtime
            .agent
            .post(self.runtime.url(&self.messages_path))
            .header("anthropic-version", &self.api_version)
            .header("content-type", "application/json");
        for (name, value) in &self.default_headers {
            http_request = http_request.header(name, value);
        }
        if self.provider_id == "github-copilot" {
            let initiator = if request
                .messages
                .last()
                .is_some_and(|message| message.role == MessageRole::User)
            {
                "user"
            } else {
                "agent"
            };
            http_request = http_request
                .header("x-initiator", initiator)
                .header("openai-intent", "conversation-edits");
            if request.messages.iter().any(|message| {
                message
                    .content
                    .iter()
                    .any(|block| matches!(block, ContentBlock::Image { .. }))
            }) {
                http_request = http_request.header("copilot-vision-request", "true");
            }
        }
        let secret = credential.expose_utf8()?;
        let oauth = secret.starts_with("sk-ant-oat");
        if oauth {
            http_request = http_request
                .header("anthropic-beta", "claude-code-20250219,oauth-2025-04-20")
                .header("x-app", "cli")
                .header("user-agent", "keith-agent/0.1");
        }
        let credential_header = if oauth {
            "authorization"
        } else {
            &self.credential_header
        };
        let response = if self.bearer_authentication || oauth {
            http_request
                .header(credential_header, &format!("Bearer {secret}"))
                .send(&body)
        } else {
            http_request.header(credential_header, secret).send(&body)
        }
        .map_err(map_http_error)?;
        let response = check_provider_response(response, self.runtime.config.max_response_bytes)?;
        emit(
            sink,
            cancellation,
            ModelEvent::Started {
                provider_request_id: request_header(&response, "request-id"),
                model: request.model.clone(),
            },
        )?;
        parse_anthropic_stream(
            response,
            request,
            cancellation,
            sink,
            self.runtime.config.max_response_bytes,
        )
    }
}

impl ModelProvider for AnthropicProvider {
    fn provider_id(&self) -> &str {
        &self.provider_id
    }

    fn list_models(
        &self,
        credential: &ProviderCredential,
    ) -> Result<Vec<ModelDescriptor>, ProviderError> {
        if let Some(path) = &self.models_path {
            let secret = credential.expose_utf8()?;
            let oauth = secret.starts_with("sk-ant-oat");
            let mut request = self
                .runtime
                .agent
                .get(self.runtime.url(path))
                .header("anthropic-version", &self.api_version);
            for (name, value) in &self.default_headers {
                request = request.header(name, value);
            }
            if oauth {
                request = request
                    .header("authorization", &format!("Bearer {secret}"))
                    .header("anthropic-beta", "claude-code-20250219,oauth-2025-04-20")
                    .header("x-app", "cli")
                    .header("user-agent", "keith-agent/0.1");
            } else if self.bearer_authentication {
                request = request.header(&self.credential_header, &format!("Bearer {secret}"));
            } else {
                request = request.header(&self.credential_header, secret);
            }
            let mut response = request.call().map_err(map_http_error)?;
            check_status(response.status().as_u16())?;
            let bytes = read_bounded(&mut response, self.runtime.config.max_response_bytes)?;
            let value: Value = serde_json::from_slice(&bytes).map_err(malformed)?;
            model_list(&value, self.provider_id())
        } else {
            Ok(vec![catalog_descriptor(
                self.provider_id(),
                self.catalog_model.as_deref().ok_or_else(|| {
                    ProviderError::new(
                        ProviderErrorKind::Internal,
                        "compatible provider has no catalog model",
                    )
                })?,
            )])
        }
    }

    fn stream(
        &self,
        request: &ModelRequest,
        credential: &ProviderCredential,
        cancellation: &CancellationToken,
        sink: &mut dyn ModelEventSink,
    ) -> Result<Usage, ProviderError> {
        validate_request(request)?;
        cancellation.check()?;
        self.runtime.register(&request.request_id, cancellation)?;
        let result = self.stream_inner(request, credential, cancellation, sink);
        self.runtime.unregister(&request.request_id);
        result
    }

    fn count_tokens(
        &self,
        request: &ModelRequest,
        _credential: &ProviderCredential,
    ) -> Result<u64, ProviderError> {
        validate_request(request)?;
        approximate_token_count(request)
    }

    fn cancel(&self, request_id: &EntityId) -> Result<(), ProviderError> {
        self.runtime.cancel(request_id)
    }
}

fn openai_request(request: &ModelRequest) -> Result<Vec<u8>, ProviderError> {
    let mut messages = Vec::new();
    if !request.system.is_empty() {
        messages.push(json!({
            "role": "system",
            "content": openai_content(&request.system),
        }));
    }
    for message in &request.messages {
        messages.extend(openai_messages(message));
    }
    let tools = request
        .tools
        .iter()
        .map(|tool| {
            json!({
                "type": "function",
                "function": {
                    "name": tool.name,
                    "description": tool.description,
                    "parameters": tool.input_schema,
                    "x-keith-behavior": match tool.behavior {
                        ToolBehavior::ReadOnly => "read_only",
                        ToolBehavior::StateChanging => "state_changing",
                    },
                }
            })
        })
        .collect::<Vec<_>>();
    let mut body = json!({
        "model": request.model,
        "messages": messages,
        "tools": tools,
        "stream": true,
        "stream_options": {"include_usage": true},
    });
    if let Some(max_tokens) = request.max_output_tokens {
        body["max_completion_tokens"] = json!(max_tokens);
    }
    if let Some(temperature) = request.temperature {
        body["temperature"] = json!(temperature);
    }
    if let Some(effort) = &request.reasoning_effort {
        body["reasoning_effort"] = json!(effort);
    }
    serde_json::to_vec(&body).map_err(internal)
}

fn openai_responses_request(request: &ModelRequest) -> Result<Vec<u8>, ProviderError> {
    let instructions = request
        .system
        .iter()
        .filter_map(|block| match block {
            ContentBlock::Text { text } => Some(text.as_str()),
            _ => None,
        })
        .collect::<Vec<_>>()
        .join("\n");
    let input = request
        .messages
        .iter()
        .flat_map(openai_response_items)
        .collect::<Vec<_>>();
    let tools = request
        .tools
        .iter()
        .map(|tool| {
            json!({
                "type": "function",
                "name": tool.name,
                "description": tool.description,
                "parameters": tool.input_schema,
                "strict": false,
            })
        })
        .collect::<Vec<_>>();
    let mut body = json!({
        "model": request.model,
        "instructions": instructions,
        "input": input,
        "tools": tools,
        "tool_choice": "auto",
        "parallel_tool_calls": true,
        "store": false,
        "stream": true,
    });
    if let Some(max_tokens) = request.max_output_tokens {
        body["max_output_tokens"] = json!(max_tokens);
    }
    if let Some(effort) = &request.reasoning_effort {
        body["reasoning"] = json!({"effort": effort, "summary": "auto"});
    }
    serde_json::to_vec(&body).map_err(internal)
}

fn openai_response_items(message: &Message) -> Vec<Value> {
    let role = match message.role {
        MessageRole::System => "system",
        MessageRole::User | MessageRole::Tool => "user",
        MessageRole::Assistant => "assistant",
    };
    let mut content = Vec::new();
    let mut items = Vec::new();
    for block in &message.content {
        match block {
            ContentBlock::Text { text } => content.push(json!({
                "type": if role == "assistant" {"output_text"} else {"input_text"},
                "text": text,
            })),
            ContentBlock::Image { media_type, data } => content.push(json!({
                "type": "input_image",
                "image_url": format!("data:{media_type};base64,{data}"),
            })),
            ContentBlock::ToolCall {
                id,
                name,
                arguments,
            } => items.push(json!({
                "type": "function_call",
                "call_id": id.to_string(),
                "name": name,
                "arguments": arguments.to_string(),
            })),
            ContentBlock::ToolResult {
                call_id,
                content,
                is_error,
            } => items.push(json!({
                "type": "function_call_output",
                "call_id": call_id.to_string(),
                "output": if *is_error { format!("ERROR: {content}") } else { content.clone() },
            })),
        }
    }
    if !content.is_empty() {
        items.insert(0, json!({"role": role, "content": content}));
    }
    items
}

fn bedrock_request(request: &ModelRequest) -> Result<Vec<u8>, ProviderError> {
    let system = request
        .system
        .iter()
        .filter_map(|block| match block {
            ContentBlock::Text { text } => Some(json!({"text": text})),
            ContentBlock::Image { .. }
            | ContentBlock::ToolCall { .. }
            | ContentBlock::ToolResult { .. } => None,
        })
        .collect::<Vec<_>>();
    let messages = request
        .messages
        .iter()
        .map(|message| {
            json!({
                "role": if message.role == MessageRole::Assistant { "assistant" } else { "user" },
                "content": message.content.iter().map(bedrock_content).collect::<Vec<_>>(),
            })
        })
        .collect::<Vec<_>>();
    let tools = request
        .tools
        .iter()
        .map(|tool| {
            json!({
                "toolSpec": {
                    "name": tool.name,
                    "description": tool.description,
                    "inputSchema": {"json": tool.input_schema},
                }
            })
        })
        .collect::<Vec<_>>();
    let mut body = json!({
        "system": system,
        "messages": messages,
        "inferenceConfig": {
            "maxTokens": request.max_output_tokens.unwrap_or(4096),
        },
    });
    if let Some(temperature) = request.temperature {
        body["inferenceConfig"]["temperature"] = json!(temperature);
    }
    if !tools.is_empty() {
        body["toolConfig"] = json!({"tools": tools, "toolChoice": {"auto": {}}});
    }
    serde_json::to_vec(&body).map_err(internal)
}

fn bedrock_content(block: &ContentBlock) -> Value {
    match block {
        ContentBlock::Text { text } => json!({"text": text}),
        ContentBlock::Image { media_type, data } => {
            let format = media_type.rsplit('/').next().unwrap_or("png");
            json!({"image": {"format": format, "source": {"bytes": data}}})
        }
        ContentBlock::ToolCall {
            id,
            name,
            arguments,
        } => json!({
            "toolUse": {
                "toolUseId": id.to_string(),
                "name": name,
                "input": arguments,
            }
        }),
        ContentBlock::ToolResult {
            call_id,
            content,
            is_error,
        } => json!({
            "toolResult": {
                "toolUseId": call_id.to_string(),
                "content": [{"text": content}],
                "status": if *is_error {"error"} else {"success"},
            }
        }),
    }
}

fn normalize_bedrock_response(
    value: &Value,
    cancellation: &CancellationToken,
    sink: &mut dyn ModelEventSink,
) -> Result<Usage, ProviderError> {
    let content = value["output"]["message"]["content"]
        .as_array()
        .ok_or_else(|| {
            ProviderError::new(
                ProviderErrorKind::MalformedResponse,
                "Bedrock response has no assistant content",
            )
        })?;
    for block in content {
        if let Some(text) = block["text"].as_str() {
            emit(
                sink,
                cancellation,
                ModelEvent::TextDelta { text: text.into() },
            )?;
        }
        if let Some(tool) = block.get("toolUse") {
            let id = ToolCallId::new();
            let name = tool["name"].as_str().unwrap_or_default().to_owned();
            let arguments = tool.get("input").cloned().unwrap_or_else(|| json!({}));
            let delta = serde_json::to_string(&arguments).map_err(internal)?;
            emit(
                sink,
                cancellation,
                ModelEvent::ToolCallStarted {
                    id: id.clone(),
                    name: name.clone(),
                },
            )?;
            emit(
                sink,
                cancellation,
                ModelEvent::ToolCallArgumentsDelta {
                    id: id.clone(),
                    delta,
                },
            )?;
            emit(
                sink,
                cancellation,
                ModelEvent::ToolCallCompleted {
                    id,
                    name,
                    arguments,
                },
            )?;
        }
    }
    let usage = Usage {
        input_tokens: value["usage"]["inputTokens"].as_u64().unwrap_or(0),
        output_tokens: value["usage"]["outputTokens"].as_u64().unwrap_or(0),
        cached_input_tokens: value["usage"]["cacheReadInputTokens"].as_u64().unwrap_or(0),
    };
    emit(sink, cancellation, ModelEvent::Usage { usage })?;
    emit(
        sink,
        cancellation,
        ModelEvent::Finished {
            reason: bedrock_stop_reason(value["stopReason"].as_str().unwrap_or_default()),
        },
    )?;
    Ok(usage)
}

fn codex_account_id(token: &str) -> Result<String, ProviderError> {
    let payload = token.split('.').nth(1).ok_or_else(|| {
        ProviderError::new(
            ProviderErrorKind::Authentication,
            "Codex OAuth token is not a JWT",
        )
    })?;
    let decoded = decode_base64_url(payload)?;
    let value: Value = serde_json::from_slice(&decoded).map_err(|_| {
        ProviderError::new(
            ProviderErrorKind::Authentication,
            "Codex OAuth token payload is invalid",
        )
    })?;
    value["https://api.openai.com/auth"]["chatgpt_account_id"]
        .as_str()
        .map(str::to_owned)
        .ok_or_else(|| {
            ProviderError::new(
                ProviderErrorKind::Authentication,
                "Codex OAuth token has no account identity",
            )
        })
}

fn decode_base64_url(encoded: &str) -> Result<Vec<u8>, ProviderError> {
    let mut output = Vec::with_capacity(encoded.len().saturating_mul(3) / 4);
    let mut buffer = 0_u32;
    let mut bits = 0_u8;
    for byte in encoded.bytes().filter(|byte| *byte != b'=') {
        let value = match byte {
            b'A'..=b'Z' => byte - b'A',
            b'a'..=b'z' => byte - b'a' + 26,
            b'0'..=b'9' => byte - b'0' + 52,
            b'-' => 62,
            b'_' => 63,
            _ => {
                return Err(ProviderError::new(
                    ProviderErrorKind::Authentication,
                    "Codex OAuth token encoding is invalid",
                ));
            }
        };
        buffer = (buffer << 6) | u32::from(value);
        bits = bits.saturating_add(6);
        if bits >= 8 {
            bits -= 8;
            output.push(
                u8::try_from((buffer >> bits) & 0xff).expect("base64 output is masked to one byte"),
            );
            buffer &= (1_u32 << bits).saturating_sub(1);
        }
    }
    Ok(output)
}

fn anthropic_request(request: &ModelRequest) -> Result<Vec<u8>, ProviderError> {
    let system = anthropic_content(&request.system);
    let messages = request
        .messages
        .iter()
        .map(anthropic_message)
        .collect::<Vec<_>>();
    let tools = request
        .tools
        .iter()
        .map(|tool| {
            json!({
                "name": tool.name,
                "description": tool.description,
                "input_schema": tool.input_schema,
            })
        })
        .collect::<Vec<_>>();
    let body = json!({
        "model": request.model,
        "system": system,
        "messages": messages,
        "tools": tools,
        "stream": true,
        "max_tokens": request.max_output_tokens.unwrap_or(4096),
        "temperature": request.temperature,
    });
    serde_json::to_vec(&body).map_err(internal)
}

fn openai_messages(message: &Message) -> Vec<Value> {
    let role = match message.role {
        MessageRole::System => "system",
        MessageRole::User => "user",
        MessageRole::Assistant => "assistant",
        MessageRole::Tool => "tool",
    };
    let mut result = Vec::new();
    let regular = message
        .content
        .iter()
        .filter(|block| !matches!(block, ContentBlock::ToolResult { .. }))
        .cloned()
        .collect::<Vec<_>>();
    if !regular.is_empty() {
        let tool_calls = regular
            .iter()
            .filter_map(|block| match block {
                ContentBlock::ToolCall {
                    id,
                    name,
                    arguments,
                } => Some(json!({
                    "id": id.to_string(),
                    "type": "function",
                    "function": {"name": name, "arguments": arguments.to_string()},
                })),
                ContentBlock::Text { .. }
                | ContentBlock::Image { .. }
                | ContentBlock::ToolResult { .. } => None,
            })
            .collect::<Vec<_>>();
        let content = openai_content(&regular);
        result.push(json!({"role": role, "content": content, "tool_calls": tool_calls}));
    }
    for block in &message.content {
        if let ContentBlock::ToolResult {
            call_id, content, ..
        } = block
        {
            result.push(json!({
                "role": "tool",
                "tool_call_id": call_id.to_string(),
                "content": content,
            }));
        }
    }
    result
}

fn openai_content(content: &[ContentBlock]) -> Value {
    let parts = content
        .iter()
        .filter_map(|block| match block {
            ContentBlock::Text { text } => Some(json!({"type": "text", "text": text})),
            ContentBlock::Image { media_type, data } => Some(json!({
                "type": "image_url",
                "image_url": {"url": format!("data:{media_type};base64,{data}")}
            })),
            ContentBlock::ToolCall { .. } | ContentBlock::ToolResult { .. } => None,
        })
        .collect::<Vec<_>>();
    Value::Array(parts)
}

fn anthropic_message(message: &Message) -> Value {
    let role = if message.role == MessageRole::Assistant {
        "assistant"
    } else {
        "user"
    };
    json!({"role": role, "content": anthropic_content(&message.content)})
}

fn anthropic_content(content: &[ContentBlock]) -> Value {
    Value::Array(
        content
            .iter()
            .map(|block| match block {
                ContentBlock::Text { text } => json!({"type": "text", "text": text}),
                ContentBlock::Image { media_type, data } => json!({
                    "type": "image",
                    "source": {"type": "base64", "media_type": media_type, "data": data}
                }),
                ContentBlock::ToolCall {
                    id,
                    name,
                    arguments,
                } => json!({
                    "type": "tool_use",
                    "id": id.to_string(),
                    "name": name,
                    "input": arguments,
                }),
                ContentBlock::ToolResult {
                    call_id,
                    content,
                    is_error,
                } => json!({
                    "type": "tool_result",
                    "tool_use_id": call_id.to_string(),
                    "content": content,
                    "is_error": is_error,
                }),
            })
            .collect(),
    )
}

#[allow(clippy::too_many_lines)]
fn parse_openai_responses_stream(
    mut response: Response<ureq::Body>,
    request: &ModelRequest,
    cancellation: &CancellationToken,
    sink: &mut dyn ModelEventSink,
    max_bytes: u64,
) -> Result<Usage, ProviderError> {
    let mut usage = Usage::default();
    let mut calls = BTreeMap::<usize, (ToolCallId, String, String)>::new();
    let mut started = false;
    let mut finished = false;
    let mut saw_tool_call = false;
    read_sse(&mut response, max_bytes, |data| {
        cancellation.check()?;
        if data == "[DONE]" {
            return Ok(false);
        }
        let value: Value = serde_json::from_str(data).map_err(malformed)?;
        if !started {
            emit(
                sink,
                cancellation,
                ModelEvent::Started {
                    provider_request_id: value["response"]["id"].as_str().map(str::to_owned),
                    model: request.model.clone(),
                },
            )?;
            started = true;
        }
        match value["type"].as_str().unwrap_or_default() {
            "response.output_text.delta" => {
                if let Some(text) = value["delta"].as_str() {
                    emit(
                        sink,
                        cancellation,
                        ModelEvent::TextDelta { text: text.into() },
                    )?;
                }
            }
            "response.reasoning_summary_text.delta" | "response.reasoning_text.delta" => {
                if let Some(text) = value["delta"].as_str() {
                    emit(
                        sink,
                        cancellation,
                        ModelEvent::ReasoningDelta { text: text.into() },
                    )?;
                }
            }
            "response.output_item.added" if value["item"]["type"] == "function_call" => {
                saw_tool_call = true;
                let index = response_output_index(&value);
                let id = ToolCallId::new();
                let name = value["item"]["name"]
                    .as_str()
                    .unwrap_or_default()
                    .to_owned();
                let arguments = value["item"]["arguments"]
                    .as_str()
                    .unwrap_or_default()
                    .to_owned();
                calls.insert(index, (id.clone(), name.clone(), arguments));
                emit(sink, cancellation, ModelEvent::ToolCallStarted { id, name })?;
            }
            "response.function_call_arguments.delta" => {
                let index = response_output_index(&value);
                let delta = value["delta"].as_str().unwrap_or_default();
                if let Some((id, _, arguments)) = calls.get_mut(&index) {
                    arguments.push_str(delta);
                    emit(
                        sink,
                        cancellation,
                        ModelEvent::ToolCallArgumentsDelta {
                            id: id.clone(),
                            delta: delta.into(),
                        },
                    )?;
                }
            }
            "response.output_item.done" if value["item"]["type"] == "function_call" => {
                let index = response_output_index(&value);
                if let Some((id, name, mut arguments)) = calls.remove(&index) {
                    if arguments.is_empty() {
                        value["item"]["arguments"]
                            .as_str()
                            .unwrap_or_default()
                            .clone_into(&mut arguments);
                    }
                    emit_completed_call(id, name, &arguments, cancellation, sink)?;
                }
            }
            "response.completed" => {
                usage = Usage {
                    input_tokens: value["response"]["usage"]["input_tokens"]
                        .as_u64()
                        .unwrap_or(0),
                    output_tokens: value["response"]["usage"]["output_tokens"]
                        .as_u64()
                        .unwrap_or(0),
                    cached_input_tokens:
                        value["response"]["usage"]["input_tokens_details"]["cached_tokens"]
                            .as_u64()
                            .unwrap_or(0),
                };
                emit(sink, cancellation, ModelEvent::Usage { usage })?;
                let reason = if saw_tool_call {
                    StopReason::ToolUse
                } else {
                    StopReason::EndTurn
                };
                finish_calls(&mut calls, cancellation, sink)?;
                emit(sink, cancellation, ModelEvent::Finished { reason })?;
                finished = true;
            }
            "response.failed" | "error" => {
                return Err(normalized_stream_error(
                    &value,
                    "Responses provider stream failed",
                ));
            }
            _ => {}
        }
        Ok(true)
    })?;
    if !started {
        emit(
            sink,
            cancellation,
            ModelEvent::Started {
                provider_request_id: None,
                model: request.model.clone(),
            },
        )?;
    }
    if !finished {
        finish_calls(&mut calls, cancellation, sink)?;
        emit(
            sink,
            cancellation,
            ModelEvent::Finished {
                reason: StopReason::Other,
            },
        )?;
    }
    Ok(usage)
}

fn parse_openai_stream(
    mut response: Response<ureq::Body>,
    _request: &ModelRequest,
    cancellation: &CancellationToken,
    sink: &mut dyn ModelEventSink,
    max_bytes: u64,
) -> Result<Usage, ProviderError> {
    let mut usage = Usage::default();
    let mut calls = BTreeMap::<usize, (ToolCallId, String, String)>::new();
    let mut finished = false;
    read_sse(&mut response, max_bytes, |data| {
        handle_openai_event(
            data,
            &mut usage,
            &mut calls,
            &mut finished,
            cancellation,
            sink,
        )
    })?;
    if !calls.is_empty() {
        finish_calls(&mut calls, cancellation, sink)?;
    }
    if !finished {
        emit(
            sink,
            cancellation,
            ModelEvent::Finished {
                reason: StopReason::Other,
            },
        )?;
    }
    Ok(usage)
}

fn parse_anthropic_stream(
    mut response: Response<ureq::Body>,
    _request: &ModelRequest,
    cancellation: &CancellationToken,
    sink: &mut dyn ModelEventSink,
    max_bytes: u64,
) -> Result<Usage, ProviderError> {
    let mut usage = Usage::default();
    let mut calls = BTreeMap::<usize, (ToolCallId, String, String)>::new();
    let mut finished = false;
    read_sse(&mut response, max_bytes, |data| {
        handle_anthropic_event(
            data,
            &mut usage,
            &mut calls,
            &mut finished,
            cancellation,
            sink,
        )
    })?;
    for (_, (id, name, arguments)) in calls {
        emit_completed_call(id, name, &arguments, cancellation, sink)?;
    }
    if !finished {
        emit(
            sink,
            cancellation,
            ModelEvent::Finished {
                reason: StopReason::Other,
            },
        )?;
    }
    Ok(usage)
}

fn handle_openai_event(
    data: &str,
    usage: &mut Usage,
    calls: &mut BTreeMap<usize, (ToolCallId, String, String)>,
    finished: &mut bool,
    cancellation: &CancellationToken,
    sink: &mut dyn ModelEventSink,
) -> Result<bool, ProviderError> {
    cancellation.check()?;
    if data == "[DONE]" {
        return Ok(false);
    }
    let value: Value = serde_json::from_str(data).map_err(malformed)?;
    if value.get("error").is_some() {
        return Err(normalized_stream_error(&value, "provider stream failed"));
    }
    if let Some(raw_usage) = value.get("usage").filter(|value| !value.is_null()) {
        *usage = usage_from_openai(raw_usage);
        emit(sink, cancellation, ModelEvent::Usage { usage: *usage })?;
    }
    let Some(choice) = value["choices"]
        .as_array()
        .and_then(|values| values.first())
    else {
        return Ok(true);
    };
    let delta = &choice["delta"];
    if let Some(text) = delta["content"].as_str() {
        emit(
            sink,
            cancellation,
            ModelEvent::TextDelta { text: text.into() },
        )?;
    }
    if let Some(text) = delta["reasoning_content"].as_str() {
        emit(
            sink,
            cancellation,
            ModelEvent::ReasoningDelta { text: text.into() },
        )?;
    }
    if let Some(tool_calls) = delta["tool_calls"].as_array() {
        handle_openai_tools(tool_calls, calls, cancellation, sink)?;
    }
    if let Some(reason) = choice["finish_reason"].as_str() {
        if reason == "tool_calls" {
            finish_calls(calls, cancellation, sink)?;
        }
        emit(
            sink,
            cancellation,
            ModelEvent::Finished {
                reason: openai_stop_reason(reason),
            },
        )?;
        *finished = true;
    }
    Ok(true)
}

fn handle_openai_tools(
    tool_calls: &[Value],
    calls: &mut BTreeMap<usize, (ToolCallId, String, String)>,
    cancellation: &CancellationToken,
    sink: &mut dyn ModelEventSink,
) -> Result<(), ProviderError> {
    for tool in tool_calls {
        let index = stream_index(tool);
        let name = tool["function"]["name"].as_str().unwrap_or_default();
        match calls.entry(index) {
            Entry::Vacant(slot) => {
                let id = ToolCallId::new();
                slot.insert((id.clone(), name.into(), String::new()));
                emit(
                    sink,
                    cancellation,
                    ModelEvent::ToolCallStarted {
                        id,
                        name: name.into(),
                    },
                )?;
            }
            Entry::Occupied(mut slot) if !name.is_empty() => {
                slot.get_mut().1 = name.into();
            }
            Entry::Occupied(_) => {}
        }
        if let Some(arguments) = tool["function"]["arguments"].as_str()
            && !arguments.is_empty()
        {
            let (id, _, assembled) = calls.get_mut(&index).ok_or_else(|| {
                ProviderError::new(
                    ProviderErrorKind::MalformedResponse,
                    "tool call index disappeared during assembly",
                )
            })?;
            assembled.push_str(arguments);
            emit(
                sink,
                cancellation,
                ModelEvent::ToolCallArgumentsDelta {
                    id: id.clone(),
                    delta: arguments.into(),
                },
            )?;
        }
    }
    Ok(())
}

fn handle_anthropic_event(
    data: &str,
    usage: &mut Usage,
    calls: &mut BTreeMap<usize, (ToolCallId, String, String)>,
    finished: &mut bool,
    cancellation: &CancellationToken,
    sink: &mut dyn ModelEventSink,
) -> Result<bool, ProviderError> {
    cancellation.check()?;
    let value: Value = serde_json::from_str(data).map_err(malformed)?;
    match value["type"].as_str().unwrap_or_default() {
        "message_start" => {
            usage.input_tokens = value["message"]["usage"]["input_tokens"]
                .as_u64()
                .unwrap_or(0);
        }
        "content_block_start" => start_anthropic_block(&value, calls, cancellation, sink)?,
        "content_block_delta" => {
            handle_anthropic_delta(&value, calls, cancellation, sink)?;
        }
        "content_block_stop" => {
            let index = stream_index(&value);
            if let Some((id, name, arguments)) = calls.remove(&index) {
                emit_completed_call(id, name, &arguments, cancellation, sink)?;
            }
        }
        "message_delta" => {
            usage.output_tokens = value["usage"]["output_tokens"].as_u64().unwrap_or(0);
            emit(sink, cancellation, ModelEvent::Usage { usage: *usage })?;
            if let Some(reason) = value["delta"]["stop_reason"].as_str() {
                emit(
                    sink,
                    cancellation,
                    ModelEvent::Finished {
                        reason: anthropic_stop_reason(reason),
                    },
                )?;
                *finished = true;
            }
        }
        "error" => {
            return Err(normalized_stream_error(&value, "provider stream failed"));
        }
        _ => {}
    }
    Ok(true)
}

fn start_anthropic_block(
    value: &Value,
    calls: &mut BTreeMap<usize, (ToolCallId, String, String)>,
    cancellation: &CancellationToken,
    sink: &mut dyn ModelEventSink,
) -> Result<(), ProviderError> {
    if value["content_block"]["type"] == "tool_use" {
        let index = stream_index(value);
        let id = ToolCallId::new();
        let name = value["content_block"]["name"]
            .as_str()
            .unwrap_or_default()
            .to_owned();
        calls.insert(index, (id.clone(), name.clone(), String::new()));
        emit(sink, cancellation, ModelEvent::ToolCallStarted { id, name })?;
    }
    Ok(())
}

fn handle_anthropic_delta(
    value: &Value,
    calls: &mut BTreeMap<usize, (ToolCallId, String, String)>,
    cancellation: &CancellationToken,
    sink: &mut dyn ModelEventSink,
) -> Result<(), ProviderError> {
    match value["delta"]["type"].as_str().unwrap_or_default() {
        "text_delta" => {
            if let Some(text) = value["delta"]["text"].as_str() {
                emit(
                    sink,
                    cancellation,
                    ModelEvent::TextDelta { text: text.into() },
                )?;
            }
        }
        "thinking_delta" => {
            if let Some(text) = value["delta"]["thinking"].as_str() {
                emit(
                    sink,
                    cancellation,
                    ModelEvent::ReasoningDelta { text: text.into() },
                )?;
            }
        }
        "input_json_delta" => {
            let index = stream_index(value);
            let delta = value["delta"]["partial_json"].as_str().unwrap_or_default();
            let (id, _, assembled) = calls.get_mut(&index).ok_or_else(|| {
                ProviderError::new(
                    ProviderErrorKind::MalformedResponse,
                    "tool arguments arrived before tool start",
                )
            })?;
            assembled.push_str(delta);
            emit(
                sink,
                cancellation,
                ModelEvent::ToolCallArgumentsDelta {
                    id: id.clone(),
                    delta: delta.into(),
                },
            )?;
        }
        _ => {}
    }
    Ok(())
}

fn read_sse(
    response: &mut Response<ureq::Body>,
    max_bytes: u64,
    mut consume: impl FnMut(&str) -> Result<bool, ProviderError>,
) -> Result<(), ProviderError> {
    let mut reader = BufReader::new(response.body_mut().as_reader());
    let mut total = 0_u64;
    loop {
        let mut line = String::new();
        let read = reader.read_line(&mut line).map_err(transport_io)?;
        if read == 0 {
            break;
        }
        total = total
            .checked_add(u64::try_from(read).map_err(|_| response_too_large())?)
            .ok_or_else(response_too_large)?;
        if total > max_bytes {
            return Err(response_too_large());
        }
        if let Some(data) = line.strip_prefix("data:")
            && !consume(data.trim())?
        {
            break;
        }
    }
    Ok(())
}

fn finish_calls(
    calls: &mut BTreeMap<usize, (ToolCallId, String, String)>,
    cancellation: &CancellationToken,
    sink: &mut dyn ModelEventSink,
) -> Result<(), ProviderError> {
    let drained = std::mem::take(calls);
    for (_, (id, name, arguments)) in drained {
        emit_completed_call(id, name, &arguments, cancellation, sink)?;
    }
    Ok(())
}

fn emit_completed_call(
    id: ToolCallId,
    name: String,
    arguments: &str,
    cancellation: &CancellationToken,
    sink: &mut dyn ModelEventSink,
) -> Result<(), ProviderError> {
    let arguments = if arguments.trim().is_empty() {
        json!({})
    } else {
        serde_json::from_str(arguments).map_err(malformed)?
    };
    emit(
        sink,
        cancellation,
        ModelEvent::ToolCallCompleted {
            id,
            name,
            arguments,
        },
    )
}

fn model_list(value: &Value, provider: &str) -> Result<Vec<ModelDescriptor>, ProviderError> {
    let data = value["data"].as_array().ok_or_else(|| {
        ProviderError::new(
            ProviderErrorKind::MalformedResponse,
            "model discovery response has no data array",
        )
    })?;
    let mut models = data
        .iter()
        .filter_map(|model| model["id"].as_str())
        .map(|id| ModelDescriptor {
            provider: provider.into(),
            id: id.into(),
            display_name: id.into(),
            context_tokens: None,
            output_tokens: None,
            supports_tools: true,
            supports_reasoning: true,
            supports_vision: true,
        })
        .collect::<Vec<_>>();
    models.sort_by(|left, right| left.id.cmp(&right.id));
    Ok(models)
}

fn catalog_descriptor(provider: &str, model: &str) -> ModelDescriptor {
    ModelDescriptor {
        provider: provider.into(),
        id: model.into(),
        display_name: model.into(),
        context_tokens: None,
        output_tokens: None,
        supports_tools: true,
        supports_reasoning: true,
        supports_vision: true,
    }
}

fn usage_from_openai(value: &Value) -> Usage {
    Usage {
        input_tokens: value["prompt_tokens"].as_u64().unwrap_or(0),
        output_tokens: value["completion_tokens"].as_u64().unwrap_or(0),
        cached_input_tokens: value["prompt_tokens_details"]["cached_tokens"]
            .as_u64()
            .unwrap_or(0),
    }
}

fn openai_stop_reason(reason: &str) -> StopReason {
    match reason {
        "stop" => StopReason::EndTurn,
        "tool_calls" | "function_call" => StopReason::ToolUse,
        "length" => StopReason::MaxTokens,
        "content_filter" => StopReason::ContentRejected,
        _ => StopReason::Other,
    }
}

fn anthropic_stop_reason(reason: &str) -> StopReason {
    match reason {
        "end_turn" | "stop_sequence" => StopReason::EndTurn,
        "tool_use" => StopReason::ToolUse,
        "max_tokens" => StopReason::MaxTokens,
        _ => StopReason::Other,
    }
}

fn bedrock_stop_reason(reason: &str) -> StopReason {
    match reason {
        "end_turn" | "stop_sequence" => StopReason::EndTurn,
        "tool_use" => StopReason::ToolUse,
        "max_tokens" => StopReason::MaxTokens,
        "content_filtered" | "guardrail_intervened" => StopReason::ContentRejected,
        _ => StopReason::Other,
    }
}

fn percent_encode_path_segment(value: &str) -> String {
    let mut encoded = String::with_capacity(value.len());
    for byte in value.bytes() {
        if byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.' | b'~') {
            encoded.push(char::from(byte));
        } else {
            use std::fmt::Write as _;
            write!(encoded, "%{byte:02X}").expect("writing to a String cannot fail");
        }
    }
    encoded
}

fn stream_index(value: &Value) -> usize {
    value["index"]
        .as_u64()
        .and_then(|index| usize::try_from(index).ok())
        .unwrap_or(0)
}

fn response_output_index(value: &Value) -> usize {
    value["output_index"]
        .as_u64()
        .and_then(|index| usize::try_from(index).ok())
        .unwrap_or(0)
}

fn read_bounded(
    response: &mut Response<ureq::Body>,
    max_bytes: u64,
) -> Result<Vec<u8>, ProviderError> {
    response
        .body_mut()
        .with_config()
        .limit(max_bytes)
        .read_to_vec()
        .map_err(map_http_error)
}

fn request_header(response: &Response<ureq::Body>, name: &str) -> Option<String> {
    response
        .headers()
        .get(name)
        .and_then(|value| value.to_str().ok())
        .map(str::to_owned)
}

fn check_status(status: u16) -> Result<(), ProviderError> {
    if (200..300).contains(&status) {
        Ok(())
    } else {
        Err(classify_http_status(
            status,
            format!("provider returned HTTP status {status}"),
        ))
    }
}

fn check_provider_response(
    mut response: Response<ureq::Body>,
    max_bytes: u64,
) -> Result<Response<ureq::Body>, ProviderError> {
    let status = response.status().as_u16();
    if (200..300).contains(&status) {
        return Ok(response);
    }
    let body = read_bounded(&mut response, max_bytes).unwrap_or_default();
    if let Ok(value) = serde_json::from_slice::<Value>(&body) {
        let mut normalized = normalized_stream_error(&value, "provider request failed");
        if normalized.kind == ProviderErrorKind::ContextOverflow {
            normalized.provider_status = Some(status);
            normalized.message =
                "provider reported that the request exceeded its context window".into();
            return Err(normalized);
        }
    }
    Err(classify_http_status(
        status,
        format!("provider returned HTTP status {status}"),
    ))
}

fn normalized_stream_error(value: &Value, fallback: &str) -> ProviderError {
    let error = value
        .get("error")
        .or_else(|| value["response"].get("error"));
    let code = error
        .and_then(|error| error.get("code").or_else(|| error.get("type")))
        .and_then(Value::as_str)
        .unwrap_or_default();
    let message = error
        .and_then(|error| error.get("message"))
        .and_then(Value::as_str)
        .unwrap_or(fallback);
    let normalized_code = code.to_ascii_lowercase();
    let normalized_message = message.to_ascii_lowercase();
    let kind = if matches!(
        normalized_code.as_str(),
        "context_length_exceeded"
            | "context_window_exceeded"
            | "prompt_too_long"
            | "max_context_length"
    ) || normalized_message.contains("context window")
        || normalized_message.contains("context length")
        || normalized_message.contains("too many input tokens")
    {
        ProviderErrorKind::ContextOverflow
    } else {
        ProviderErrorKind::Unavailable
    };
    ProviderError::new(kind, message)
}

fn map_http_error(error: HttpError) -> ProviderError {
    match error {
        HttpError::StatusCode(status) => {
            classify_http_status(status, format!("provider returned HTTP status {status}"))
        }
        HttpError::Timeout(_) => {
            ProviderError::new(ProviderErrorKind::Timeout, "provider request timed out")
        }
        HttpError::Io(_) | HttpError::ConnectionFailed | HttpError::HostNotFound => {
            ProviderError::new(
                ProviderErrorKind::Unavailable,
                "provider transport is unavailable",
            )
        }
        other => ProviderError::new(
            ProviderErrorKind::Internal,
            format!("provider HTTP client failed: {other}"),
        ),
    }
}

fn transport_io(_error: std::io::Error) -> ProviderError {
    ProviderError::new(
        ProviderErrorKind::Unavailable,
        "provider stream transport failed",
    )
}

fn malformed(error: impl std::fmt::Display) -> ProviderError {
    ProviderError::new(ProviderErrorKind::MalformedResponse, error.to_string())
}

fn internal(error: impl std::fmt::Display) -> ProviderError {
    ProviderError::new(ProviderErrorKind::Internal, error.to_string())
}

fn response_too_large() -> ProviderError {
    ProviderError::new(
        ProviderErrorKind::MalformedResponse,
        "provider response exceeded its configured byte limit",
    )
}

#[cfg(test)]
mod tests {
    use std::io::{Read, Write};
    use std::net::{TcpListener, TcpStream};
    use std::sync::mpsc::{self, Receiver};
    use std::thread;

    use keith_provider_core::StreamControl;

    use super::*;

    struct TestServer {
        base_url: String,
        requests: Receiver<String>,
        thread: Option<thread::JoinHandle<()>>,
    }

    impl TestServer {
        fn start(responses: Vec<String>) -> Self {
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let address = listener.local_addr().unwrap();
            let (sender, requests) = mpsc::channel();
            let thread = thread::spawn(move || {
                for response in responses {
                    let (mut stream, _) = listener.accept().unwrap();
                    let request = read_request(&mut stream);
                    sender.send(request).unwrap();
                    stream.write_all(response.as_bytes()).unwrap();
                    stream.flush().unwrap();
                }
            });
            Self {
                base_url: format!("http://{address}"),
                requests,
                thread: Some(thread),
            }
        }

        fn request(&self) -> String {
            self.requests.recv_timeout(Duration::from_secs(5)).unwrap()
        }
    }

    impl Drop for TestServer {
        fn drop(&mut self) {
            if let Some(thread) = self.thread.take() {
                thread.join().unwrap();
            }
        }
    }

    fn read_request(stream: &mut TcpStream) -> String {
        stream
            .set_read_timeout(Some(Duration::from_secs(5)))
            .unwrap();
        let mut bytes = Vec::new();
        let mut buffer = [0_u8; 4096];
        loop {
            let read = stream.read(&mut buffer).unwrap();
            if read == 0 {
                break;
            }
            bytes.extend_from_slice(&buffer[..read]);
            if let Some(header_end) = bytes.windows(4).position(|window| window == b"\r\n\r\n") {
                let headers = String::from_utf8_lossy(&bytes[..header_end + 4]);
                let length = headers
                    .lines()
                    .find_map(|line| {
                        line.to_ascii_lowercase()
                            .strip_prefix("content-length:")
                            .and_then(|value| value.trim().parse::<usize>().ok())
                    })
                    .unwrap_or(0);
                if bytes.len() >= header_end + 4 + length {
                    break;
                }
            }
        }
        String::from_utf8(bytes).unwrap()
    }

    fn response(content_type: &str, body: &str) -> String {
        format!(
            "HTTP/1.1 200 OK\r\nContent-Type: {content_type}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
            body.len()
        )
    }

    fn request() -> ModelRequest {
        let system = vec![ContentBlock::Text {
            text: "system".into(),
        }];
        let messages = vec![Message {
            role: MessageRole::User,
            content: vec![ContentBlock::Text {
                text: "hello".into(),
            }],
        }];
        let context = keith_provider_core::RequestContext::synthetic(&system, &messages);
        ModelRequest {
            request_id: EntityId::new(),
            purpose: keith_provider_core::ModelRequestPurpose::Primary,
            model: "model-a".into(),
            system,
            messages,
            tools: vec![keith_provider_core::ToolDefinition {
                name: "lookup".into(),
                description: "look up a value".into(),
                input_schema: json!({"type": "object", "properties": {}}),
                behavior: ToolBehavior::ReadOnly,
            }],
            max_output_tokens: Some(100),
            temperature: None,
            reasoning_effort: None,
            context,
        }
    }

    #[test]
    fn openai_http_context_error_is_typed_before_retry_routing() {
        let body = r#"{"error":{"code":"context_length_exceeded","message":"maximum context length exceeded"}}"#;
        let server = TestServer::start(vec![format!(
            "HTTP/1.1 400 Bad Request\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
            body.len()
        )]);
        let provider =
            OpenAiProvider::new(ProviderHttpConfig::new(&server.base_url).unwrap()).unwrap();
        let error = provider
            .stream(
                &request(),
                &ProviderCredential::new("test-key").unwrap(),
                &CancellationToken::default(),
                &mut |_event| Ok(StreamControl::Continue),
            )
            .unwrap_err();
        assert_eq!(error.kind, ProviderErrorKind::ContextOverflow);
        assert_eq!(error.provider_status, Some(400));
        assert!(!error.allows_retry_or_fallback());
    }

    #[test]
    fn openai_adapter_discovers_streams_tools_usage_and_scopes_secret_to_header() {
        let model_body = r#"{"data":[{"id":"model-a"}]}"#;
        let stream_body = concat!(
            "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n",
            "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_x\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":1}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n",
            "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3}}\n\n",
            "data: [DONE]\n\n"
        );
        let server = TestServer::start(vec![
            response("application/json", model_body),
            response("text/event-stream", stream_body),
        ]);
        let provider =
            OpenAiProvider::new(ProviderHttpConfig::new(&server.base_url).unwrap()).unwrap();
        let credential = ProviderCredential::new("openai-secret").unwrap();
        assert_eq!(provider.list_models(&credential).unwrap()[0].id, "model-a");
        let first_request = server.request();
        assert!(first_request.contains("authorization: Bearer openai-secret"));
        let mut events = Vec::new();
        let mut sink = |event| {
            events.push(event);
            Ok(StreamControl::Continue)
        };
        let usage = provider
            .stream(
                &request(),
                &credential,
                &CancellationToken::default(),
                &mut sink,
            )
            .unwrap();
        let second_request = server.request();
        let body = second_request.split("\r\n\r\n").nth(1).unwrap();
        assert!(!body.contains("openai-secret"));
        assert_eq!(usage.total_tokens(), 8);
        assert!(
            events
                .iter()
                .any(|event| matches!(event, ModelEvent::ToolCallCompleted { .. }))
        );
    }

    #[test]
    fn anthropic_adapter_discovers_and_normalizes_text_reasoning_tools_and_usage() {
        let model_body = r#"{"data":[{"id":"claude-a"}]}"#;
        let stream_body = concat!(
            "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7}}}\n\n",
            "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
            "data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool_x\",\"name\":\"lookup\"}}\n\n",
            "data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":2}\"}}\n\n",
            "data: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
            "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":4}}\n\n",
            "data: {\"type\":\"message_stop\"}\n\n"
        );
        let server = TestServer::start(vec![
            response("application/json", model_body),
            response("text/event-stream", stream_body),
        ]);
        let provider =
            AnthropicProvider::new(ProviderHttpConfig::new(&server.base_url).unwrap()).unwrap();
        let credential = ProviderCredential::new("anthropic-secret").unwrap();
        assert_eq!(provider.list_models(&credential).unwrap()[0].id, "claude-a");
        assert!(server.request().contains("x-api-key: anthropic-secret"));
        let mut events = Vec::new();
        let mut sink = |event| {
            events.push(event);
            Ok(StreamControl::Continue)
        };
        let mut normalized_request = request();
        normalized_request.model = "claude-a".into();
        let usage = provider
            .stream(
                &normalized_request,
                &credential,
                &CancellationToken::default(),
                &mut sink,
            )
            .unwrap();
        assert_eq!(usage.total_tokens(), 11);
        assert!(
            events
                .iter()
                .any(|event| matches!(event, ModelEvent::TextDelta { text } if text == "hi"))
        );
        assert!(
            events
                .iter()
                .any(|event| matches!(event, ModelEvent::ToolCallCompleted { .. }))
        );
    }

    #[test]
    fn status_and_cancellation_are_typed_without_secret_leakage() {
        let server = TestServer::start(vec![
            "HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\nConnection: close\r\n\r\n".into(),
        ]);
        let provider =
            OpenAiProvider::new(ProviderHttpConfig::new(&server.base_url).unwrap()).unwrap();
        let credential = ProviderCredential::new("never-log-me").unwrap();
        let error = provider.list_models(&credential).unwrap_err();
        assert_eq!(error.kind, ProviderErrorKind::Authentication);
        assert!(!error.to_string().contains("never-log-me"));
        let token = CancellationToken::default();
        token.cancel();
        let mut sink = |_event| Ok(StreamControl::Continue);
        let error = provider
            .stream(&request(), &credential, &token, &mut sink)
            .unwrap_err();
        assert_eq!(error.kind, ProviderErrorKind::Cancelled);
        let _ = server.request();
    }

    #[test]
    fn compatible_catalog_provider_streams_with_provider_specific_identity_and_auth() {
        let stream_body = concat!(
            "data: {\"choices\":[{\"delta\":{\"content\":\"compatible\"},\"finish_reason\":\"stop\"}]}\n\n",
            "data: [DONE]\n\n"
        );
        let server = TestServer::start(vec![
            response("text/event-stream", stream_body),
            response("text/event-stream", stream_body),
        ]);
        let provider = OpenAiProvider::compatible(
            "deepseek",
            ProviderHttpConfig::new(&server.base_url).unwrap(),
            "deepseek-chat",
            false,
        )
        .unwrap();
        let credential = ProviderCredential::new("compatible-secret").unwrap();
        let models = provider.list_models(&credential).unwrap();
        assert_eq!(models[0].provider, "deepseek");
        assert_eq!(models[0].id, "deepseek-chat");
        let mut sink = |_event| Ok(StreamControl::Continue);
        provider
            .stream(
                &request(),
                &credential,
                &CancellationToken::default(),
                &mut sink,
            )
            .unwrap();
        let bearer_request = server.request();
        assert!(bearer_request.starts_with("POST /chat/completions "));
        assert!(bearer_request.contains("authorization: Bearer compatible-secret"));

        let azure = OpenAiProvider::compatible(
            "azure-openai-responses",
            ProviderHttpConfig::new(&server.base_url).unwrap(),
            "gpt-4.1",
            false,
        )
        .unwrap()
        .with_api_key_header();
        azure
            .stream(
                &request(),
                &credential,
                &CancellationToken::default(),
                &mut sink,
            )
            .unwrap();
        let api_key_request = server.request();
        assert!(api_key_request.contains("api-key: compatible-secret"));
        assert!(!api_key_request.contains("authorization: Bearer"));
    }

    #[test]
    fn anthropic_compatible_routes_apply_copilot_and_cloudflare_auth_contracts() {
        let stream_body = concat!(
            "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n",
            "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n",
            "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"
        );
        let server = TestServer::start(vec![
            response("text/event-stream", stream_body),
            response("text/event-stream", stream_body),
        ]);
        let credential = ProviderCredential::new("compatible-token").unwrap();
        let mut sink = |_event| Ok(StreamControl::Continue);

        let copilot = AnthropicProvider::compatible(
            "github-copilot",
            ProviderHttpConfig::new(&server.base_url).unwrap(),
            "claude-sonnet-4-5",
            true,
        )
        .unwrap()
        .with_default_header("editor-version", "vscode/1.107.0")
        .unwrap();
        copilot
            .stream(
                &request(),
                &credential,
                &CancellationToken::default(),
                &mut sink,
            )
            .unwrap();
        let copilot_request = server.request();
        assert!(copilot_request.contains("authorization: Bearer compatible-token"));
        assert!(copilot_request.contains("editor-version: vscode/1.107.0"));
        assert!(copilot_request.contains("x-initiator: user"));
        assert!(copilot_request.contains("openai-intent: conversation-edits"));

        let cloudflare = AnthropicProvider::compatible(
            "cloudflare-ai-gateway",
            ProviderHttpConfig::new(&server.base_url).unwrap(),
            "claude-sonnet-4-5",
            true,
        )
        .unwrap()
        .with_credential_header("cf-aig-authorization", true)
        .unwrap();
        cloudflare
            .stream(
                &request(),
                &credential,
                &CancellationToken::default(),
                &mut sink,
            )
            .unwrap();
        let cloudflare_request = server.request();
        assert!(cloudflare_request.contains("cf-aig-authorization: Bearer compatible-token"));
        assert!(
            !cloudflare_request
                .lines()
                .any(|line| line.starts_with("authorization: Bearer compatible-token"))
        );
    }

    #[test]
    fn codex_responses_adapter_streams_multiple_tools_and_scopes_oauth_headers() {
        let stream_body = concat!(
            "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n",
            "data: {\"type\":\"response.output_text.delta\",\"delta\":\"working\"}\n\n",
            "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"name\":\"lookup\",\"arguments\":\"\"}}\n\n",
            "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"{\\\"q\\\":1}\"}\n\n",
            "data: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"name\":\"lookup\",\"arguments\":\"\"}}\n\n",
            "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":1,\"delta\":\"{\\\"q\\\":2}\"}\n\n",
            "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":1}\"}}\n\n",
            "data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":2}\"}}\n\n",
            "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":3,\"input_tokens_details\":{\"cached_tokens\":2}}}}\n\n"
        );
        let server = TestServer::start(vec![response("text/event-stream", stream_body)]);
        let provider = OpenAiResponsesProvider::codex(
            ProviderHttpConfig::new(&server.base_url).unwrap(),
            "gpt-5.1",
        )
        .unwrap();
        let token = concat!(
            "e30.",
            "eyJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9hY2NvdW50X2lkIjoiYWNjdC10ZXN0In19",
            ".signature"
        );
        let credential = ProviderCredential::new(token).unwrap();
        assert_eq!(provider.list_models(&credential).unwrap()[0].id, "gpt-5.1");
        let mut events = Vec::new();
        let mut sink = |event| {
            events.push(event);
            Ok(StreamControl::Continue)
        };
        let usage = provider
            .stream(
                &request(),
                &credential,
                &CancellationToken::default(),
                &mut sink,
            )
            .unwrap();
        assert_eq!(usage.total_tokens(), 8);
        assert!(matches!(events.first(), Some(ModelEvent::Started { .. })));
        assert!(
            events
                .iter()
                .any(|event| matches!(event, ModelEvent::TextDelta { text } if text == "working"))
        );
        assert_eq!(
            events
                .iter()
                .filter(|event| matches!(event, ModelEvent::ToolCallCompleted { .. }))
                .count(),
            2
        );
        assert!(events.iter().any(|event| matches!(
            event,
            ModelEvent::Finished {
                reason: StopReason::ToolUse
            }
        )));
        let request = server.request();
        assert!(request.starts_with("POST /codex/responses "));
        assert!(request.contains(&format!("authorization: Bearer {token}")));
        assert!(request.contains("chatgpt-account-id: acct-test"));
        assert!(request.contains("openai-beta: responses=experimental"));
        assert!(!request.split("\r\n\r\n").nth(1).unwrap().contains(token));
    }

    #[test]
    fn openai_luna_uses_responses_when_reasoning_and_tools_are_enabled() {
        let stream_body = concat!(
            "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_luna\"}}\n\n",
            "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"Checking the request.\"}\n\n",
            "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Luna is live.\"}\n\n",
            "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":4,\"input_tokens_details\":{\"cached_tokens\":1}}}}\n\n"
        );
        let server = TestServer::start(vec![response("text/event-stream", stream_body)]);
        let provider =
            OpenAiProvider::new(ProviderHttpConfig::new(&server.base_url).unwrap()).unwrap();
        let credential = ProviderCredential::new("openai-secret").unwrap();
        let mut luna_request = request();
        luna_request.model = "gpt-5.6-luna".into();
        luna_request.reasoning_effort = Some("medium".into());
        let mut events = Vec::new();
        let usage = provider
            .stream(
                &luna_request,
                &credential,
                &CancellationToken::default(),
                &mut |event| {
                    events.push(event);
                    Ok(StreamControl::Continue)
                },
            )
            .unwrap();

        assert_eq!(usage.total_tokens(), 11);
        assert!(events.iter().any(|event| matches!(
            event,
            ModelEvent::ReasoningDelta { text } if text == "Checking the request."
        )));
        assert!(events.iter().any(|event| matches!(
            event,
            ModelEvent::TextDelta { text } if text == "Luna is live."
        )));
        let request = server.request();
        assert!(request.starts_with("POST /v1/responses "));
        assert!(request.contains("authorization: Bearer openai-secret"));
        let body = request.split("\r\n\r\n").nth(1).unwrap();
        assert!(body.contains("\"reasoning\":{\"effort\":\"medium\",\"summary\":\"auto\"}"));
        assert!(!body.contains("openai-secret"));
    }

    #[test]
    fn bedrock_converse_adapter_normalizes_response_and_encodes_model_path() {
        let response_body = json!({
            "output": {
                "message": {
                    "role": "assistant",
                    "content": [
                        {"text": "bedrock"},
                        {"toolUse": {"toolUseId": "call-1", "name": "lookup", "input": {"q": 4}}}
                    ]
                }
            },
            "stopReason": "tool_use",
            "usage": {"inputTokens": 9, "outputTokens": 4, "cacheReadInputTokens": 2}
        })
        .to_string();
        let server = TestServer::start(vec![response("application/json", &response_body)]);
        let provider = AmazonBedrockProvider::new(
            ProviderHttpConfig::new(&server.base_url).unwrap(),
            "us.anthropic.claude-sonnet-4-20250514-v1:0",
        )
        .unwrap();
        let credential = ProviderCredential::new("bedrock-bearer-secret").unwrap();
        let mut normalized_request = request();
        normalized_request.model = "arn:aws:bedrock:us-east-1:1:profile/model".into();
        let mut events = Vec::new();
        let mut sink = |event| {
            events.push(event);
            Ok(StreamControl::Continue)
        };
        let usage = provider
            .stream(
                &normalized_request,
                &credential,
                &CancellationToken::default(),
                &mut sink,
            )
            .unwrap();
        assert_eq!(usage.total_tokens(), 13);
        assert!(
            events
                .iter()
                .any(|event| matches!(event, ModelEvent::TextDelta { text } if text == "bedrock"))
        );
        assert!(events.iter().any(|event| matches!(
            event,
            ModelEvent::Finished {
                reason: StopReason::ToolUse
            }
        )));
        let request = server.request();
        assert!(request.starts_with(
            "POST /model/arn%3Aaws%3Abedrock%3Aus-east-1%3A1%3Aprofile%2Fmodel/converse "
        ));
        assert!(request.contains("authorization: Bearer bedrock-bearer-secret"));
        let body = request.split("\r\n\r\n").nth(1).unwrap();
        assert!(body.contains("\"toolConfig\""));
        assert!(!body.contains("bedrock-bearer-secret"));
    }
}
