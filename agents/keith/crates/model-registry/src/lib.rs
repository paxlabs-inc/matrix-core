#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet};
use std::sync::{Arc, RwLock, RwLockReadGuard, RwLockWriteGuard};

use keith_agent_types::ProfileId;
use keith_provider_core::{
    CancellationToken, ContextContractError, ModelDescriptor, ModelEvent, ModelEventSink,
    ModelProvider, ModelRequest, ProviderCredential, ProviderError, ProviderErrorKind,
    StreamControl, Usage,
};
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ModelSelection {
    pub provider: String,
    pub model: String,
    pub credential_ref: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ModelRoute {
    pub primary: ModelSelection,
    pub fallbacks: Vec<ModelSelection>,
    pub classification: Option<ModelSelection>,
    pub summarization: Option<ModelSelection>,
    pub review: Option<ModelSelection>,
    pub vision: Option<ModelSelection>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ModelPurpose {
    Primary,
    Classification,
    Summarization,
    Review,
    Vision,
}

#[derive(Clone)]
pub struct ResolvedModel {
    pub selection: ModelSelection,
    pub provider: Arc<dyn ModelProvider>,
}

#[derive(Clone)]
pub struct ResolvedRoute {
    pub profile_id: ProfileId,
    pub purpose: ModelPurpose,
    pub candidates: Vec<ResolvedModel>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ProviderAttempt {
    pub provider: String,
    pub model: String,
    pub usage: Usage,
    pub fallback_index: usize,
}

pub trait CredentialResolver: Send + Sync {
    /// # Errors
    ///
    /// Returns an authentication error when the named provider credential cannot be resolved.
    fn resolve(
        &self,
        provider: &str,
        credential_ref: Option<&str>,
    ) -> Result<ProviderCredential, ProviderError>;
}

impl<F> CredentialResolver for F
where
    F: Fn(&str, Option<&str>) -> Result<ProviderCredential, ProviderError> + Send + Sync,
{
    fn resolve(
        &self,
        provider: &str,
        credential_ref: Option<&str>,
    ) -> Result<ProviderCredential, ProviderError> {
        self(provider, credential_ref)
    }
}

#[derive(Debug, Error)]
pub enum RegistryError {
    #[error("model registry lock was poisoned")]
    LockPoisoned,
    #[error("provider {0} is already registered")]
    DuplicateProvider(String),
    #[error("provider {0} is not registered")]
    UnknownProvider(String),
    #[error("model {provider}/{model} was not discovered")]
    UnknownModel { provider: String, model: String },
    #[error("profile {0} has no model route")]
    MissingProfile(ProfileId),
    #[error("model route is invalid: {0}")]
    InvalidRoute(String),
    #[error("model request context contract is invalid: {0}")]
    InvalidContext(#[from] ContextContractError),
    #[error("provider failed: {0}")]
    Provider(#[from] ProviderError),
}

#[derive(Default)]
struct RegistryState {
    providers: BTreeMap<String, Arc<dyn ModelProvider>>,
    models: BTreeMap<(String, String), ModelDescriptor>,
    routes: BTreeMap<ProfileId, ModelRoute>,
    overrides: BTreeMap<ProfileId, ModelSelection>,
}

#[derive(Default)]
pub struct ModelRegistry {
    state: RwLock<RegistryState>,
}

impl ModelRegistry {
    pub fn new() -> Self {
        Self::default()
    }

    /// # Errors
    ///
    /// Returns an error when the provider ID is duplicated or the registry lock is poisoned.
    pub fn register_provider(&self, provider: Arc<dyn ModelProvider>) -> Result<(), RegistryError> {
        let mut state = self.write()?;
        let id = provider.provider_id().to_owned();
        if state.providers.contains_key(&id) {
            return Err(RegistryError::DuplicateProvider(id));
        }
        state.providers.insert(id, provider);
        Ok(())
    }

    /// # Errors
    ///
    /// Returns an error when the provider is missing, discovery fails, or state cannot update.
    pub fn refresh_models(
        &self,
        provider_id: &str,
        credential: &ProviderCredential,
    ) -> Result<Vec<ModelDescriptor>, RegistryError> {
        let provider = self
            .read()?
            .providers
            .get(provider_id)
            .cloned()
            .ok_or_else(|| RegistryError::UnknownProvider(provider_id.into()))?;
        let models = provider.list_models(credential)?;
        if models
            .iter()
            .any(|model| model.provider != provider_id || model.id.trim().is_empty())
        {
            return Err(RegistryError::InvalidRoute(
                "provider returned inconsistent model descriptors".into(),
            ));
        }
        let mut state = self.write()?;
        state
            .models
            .retain(|(provider, _), _| provider != provider_id);
        for model in &models {
            state
                .models
                .insert((model.provider.clone(), model.id.clone()), model.clone());
        }
        Ok(models)
    }

    /// Records a user-selected model for a registered provider. This supports providers
    /// whose APIs do not expose model discovery while preserving real request validation.
    ///
    /// # Errors
    ///
    /// Returns an error when the provider is unknown, the model is empty, or state is poisoned.
    pub fn register_configured_model(
        &self,
        provider: &str,
        model: &str,
    ) -> Result<(), RegistryError> {
        if model.trim().is_empty() {
            return Err(RegistryError::InvalidRoute(
                "configured model must be non-empty".into(),
            ));
        }
        let mut state = self.write()?;
        if !state.providers.contains_key(provider) {
            return Err(RegistryError::UnknownProvider(provider.into()));
        }
        state.models.insert(
            (provider.into(), model.into()),
            ModelDescriptor {
                provider: provider.into(),
                id: model.into(),
                display_name: model.into(),
                context_tokens: None,
                output_tokens: None,
                supports_tools: true,
                supports_reasoning: true,
                supports_vision: true,
            },
        );
        Ok(())
    }

    /// # Errors
    ///
    /// Returns an error when the route references unknown providers/models or is ambiguous.
    pub fn set_profile_route(
        &self,
        profile_id: ProfileId,
        route: ModelRoute,
    ) -> Result<(), RegistryError> {
        let mut state = self.write()?;
        validate_route(&state, &route)?;
        state.routes.insert(profile_id, route);
        Ok(())
    }

    /// # Errors
    ///
    /// Returns an error when the profile or selected provider/model is unavailable.
    pub fn set_runtime_override(
        &self,
        profile_id: &ProfileId,
        selection: ModelSelection,
    ) -> Result<(), RegistryError> {
        let mut state = self.write()?;
        if !state.routes.contains_key(profile_id) {
            return Err(RegistryError::MissingProfile(profile_id.clone()));
        }
        validate_selection(&state, &selection)?;
        state.overrides.insert(profile_id.clone(), selection);
        Ok(())
    }

    /// # Errors
    ///
    /// Returns an error when the registry state cannot be updated.
    pub fn clear_runtime_override(&self, profile_id: &ProfileId) -> Result<(), RegistryError> {
        self.write()?.overrides.remove(profile_id);
        Ok(())
    }

    /// # Errors
    ///
    /// Returns an error when the profile route or one of its provider/model targets is unavailable.
    pub fn resolve(
        &self,
        profile_id: &ProfileId,
        purpose: ModelPurpose,
    ) -> Result<ResolvedRoute, RegistryError> {
        let state = self.read()?;
        let route = state
            .routes
            .get(profile_id)
            .ok_or_else(|| RegistryError::MissingProfile(profile_id.clone()))?;
        let selected = specialized_selection(route, purpose).unwrap_or(&route.primary);
        let primary = if purpose == ModelPurpose::Primary {
            state.overrides.get(profile_id).unwrap_or(selected)
        } else {
            selected
        };
        let mut selections = vec![primary.clone()];
        if purpose == ModelPurpose::Primary || specialized_selection(route, purpose).is_none() {
            selections.extend(route.fallbacks.clone());
        }
        let candidates = selections
            .into_iter()
            .map(|selection| {
                validate_selection(&state, &selection)?;
                let provider = state
                    .providers
                    .get(&selection.provider)
                    .cloned()
                    .ok_or_else(|| RegistryError::UnknownProvider(selection.provider.clone()))?;
                Ok(ResolvedModel {
                    selection,
                    provider,
                })
            })
            .collect::<Result<Vec<_>, RegistryError>>()?;
        Ok(ResolvedRoute {
            profile_id: profile_id.clone(),
            purpose,
            candidates,
        })
    }

    /// # Errors
    ///
    /// Returns an error when routing, credential resolution, or every eligible provider fails.
    pub fn stream_with_fallback(
        &self,
        profile_id: &ProfileId,
        purpose: ModelPurpose,
        request: &ModelRequest,
        credentials: &dyn CredentialResolver,
        cancellation: &CancellationToken,
        sink: &mut dyn ModelEventSink,
    ) -> Result<ProviderAttempt, RegistryError> {
        request
            .context
            .validate(&request.system, &request.messages)?;
        let route = self.resolve(profile_id, purpose)?;
        let last_index = route.candidates.len().saturating_sub(1);
        for (index, candidate) in route.candidates.into_iter().enumerate() {
            let credential = credentials.resolve(
                &candidate.selection.provider,
                candidate.selection.credential_ref.as_deref(),
            )?;
            let mut routed_request = request.clone();
            routed_request.model.clone_from(&candidate.selection.model);
            let mut attempt_sink = AttemptSink {
                inner: sink,
                emitted_semantic_delta: false,
            };
            match candidate.provider.stream(
                &routed_request,
                &credential,
                cancellation,
                &mut attempt_sink,
            ) {
                Ok(usage) => {
                    return Ok(ProviderAttempt {
                        provider: candidate.selection.provider,
                        model: candidate.selection.model,
                        usage,
                        fallback_index: index,
                    });
                }
                Err(error)
                    if index < last_index
                        && error.allows_retry_or_fallback()
                        && !attempt_sink.emitted_semantic_delta => {}
                Err(error) => return Err(error.into()),
            }
        }
        Err(RegistryError::Provider(ProviderError::new(
            ProviderErrorKind::Unavailable,
            "model route contained no usable provider",
        )))
    }

    /// # Errors
    ///
    /// Returns an error when the registry lock is poisoned.
    pub fn models(&self) -> Result<Vec<ModelDescriptor>, RegistryError> {
        Ok(self.read()?.models.values().cloned().collect())
    }

    fn read(&self) -> Result<RwLockReadGuard<'_, RegistryState>, RegistryError> {
        self.state.read().map_err(|_| RegistryError::LockPoisoned)
    }

    fn write(&self) -> Result<RwLockWriteGuard<'_, RegistryState>, RegistryError> {
        self.state.write().map_err(|_| RegistryError::LockPoisoned)
    }
}

struct AttemptSink<'a> {
    inner: &'a mut dyn ModelEventSink,
    emitted_semantic_delta: bool,
}

impl ModelEventSink for AttemptSink<'_> {
    fn emit(&mut self, event: ModelEvent) -> Result<StreamControl, ProviderError> {
        if matches!(
            event,
            ModelEvent::TextDelta { .. }
                | ModelEvent::ReasoningDelta { .. }
                | ModelEvent::ToolCallStarted { .. }
                | ModelEvent::ToolCallArgumentsDelta { .. }
                | ModelEvent::ToolCallCompleted { .. }
        ) {
            self.emitted_semantic_delta = true;
        }
        self.inner.emit(event)
    }
}

fn validate_route(state: &RegistryState, route: &ModelRoute) -> Result<(), RegistryError> {
    let mut identities = BTreeSet::new();
    for selection in route_selections(route) {
        validate_selection(state, selection)?;
        let identity = (selection.provider.clone(), selection.model.clone());
        if !identities.insert(identity) {
            return Err(RegistryError::InvalidRoute(
                "a route cannot contain a duplicate provider/model target".into(),
            ));
        }
    }
    Ok(())
}

fn validate_selection(
    state: &RegistryState,
    selection: &ModelSelection,
) -> Result<(), RegistryError> {
    if selection.provider.trim().is_empty()
        || selection.model.trim().is_empty()
        || selection
            .credential_ref
            .as_ref()
            .is_some_and(|reference| reference.trim().is_empty())
    {
        return Err(RegistryError::InvalidRoute(
            "provider, model, and any credential reference must be non-empty".into(),
        ));
    }
    if !state.providers.contains_key(&selection.provider) {
        return Err(RegistryError::UnknownProvider(selection.provider.clone()));
    }
    if !state
        .models
        .contains_key(&(selection.provider.clone(), selection.model.clone()))
    {
        return Err(RegistryError::UnknownModel {
            provider: selection.provider.clone(),
            model: selection.model.clone(),
        });
    }
    Ok(())
}

fn route_selections(route: &ModelRoute) -> Vec<&ModelSelection> {
    let mut selections = vec![&route.primary];
    selections.extend(&route.fallbacks);
    selections.extend(
        [
            route.classification.as_ref(),
            route.summarization.as_ref(),
            route.review.as_ref(),
            route.vision.as_ref(),
        ]
        .into_iter()
        .flatten(),
    );
    selections
}

fn specialized_selection(route: &ModelRoute, purpose: ModelPurpose) -> Option<&ModelSelection> {
    match purpose {
        ModelPurpose::Primary => None,
        ModelPurpose::Classification => route.classification.as_ref(),
        ModelPurpose::Summarization => route.summarization.as_ref(),
        ModelPurpose::Review => route.review.as_ref(),
        ModelPurpose::Vision => route.vision.as_ref(),
    }
}

#[cfg(test)]
mod tests {
    use std::io::{Read, Write};
    use std::net::{TcpListener, TcpStream};
    use std::thread;
    use std::time::Duration;

    use keith_provider_adapters::{AnthropicProvider, OpenAiProvider, ProviderHttpConfig};

    use super::*;

    fn discovery_server(model: &str) -> (String, thread::JoinHandle<()>) {
        response_server(vec![json_response(&format!(
            r#"{{"data":[{{"id":"{model}"}}]}}"#
        ))])
    }

    fn response_server(responses: Vec<String>) -> (String, thread::JoinHandle<()>) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let thread = thread::spawn(move || {
            for response in responses {
                let (mut stream, _) = listener.accept().unwrap();
                read_request(&mut stream);
                stream.write_all(response.as_bytes()).unwrap();
            }
        });
        (format!("http://{address}"), thread)
    }

    fn json_response(body: &str) -> String {
        http_response("200 OK", "application/json", body)
    }

    fn http_response(status: &str, content_type: &str, body: &str) -> String {
        format!(
            "HTTP/1.1 {status}\r\nContent-Type: {content_type}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
            body.len()
        )
    }

    fn read_request(stream: &mut TcpStream) {
        stream
            .set_read_timeout(Some(Duration::from_secs(5)))
            .unwrap();
        let mut bytes = [0_u8; 4096];
        let _ = stream.read(&mut bytes).unwrap();
    }

    fn selection(provider: &str, model: &str, credential_ref: &str) -> ModelSelection {
        ModelSelection {
            provider: provider.into(),
            model: model.into(),
            credential_ref: Some(credential_ref.into()),
        }
    }

    fn request() -> ModelRequest {
        let system = Vec::new();
        let messages = vec![keith_provider_core::Message {
            role: keith_provider_core::MessageRole::User,
            content: vec![keith_provider_core::ContentBlock::Text {
                text: "hello".into(),
            }],
        }];
        let context = keith_provider_core::RequestContext::synthetic(&system, &messages);
        ModelRequest {
            request_id: keith_agent_types::EntityId::new(),
            purpose: keith_provider_core::ModelRequestPurpose::Primary,
            model: "route-overwrites-this".into(),
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
    fn real_adapters_discover_and_routes_remain_profile_isolated() {
        let (openai_url, openai_thread) = discovery_server("gpt-a");
        let (anthropic_url, anthropic_thread) = discovery_server("claude-a");
        let registry = ModelRegistry::new();
        registry
            .register_provider(Arc::new(
                OpenAiProvider::new(ProviderHttpConfig::new(openai_url).unwrap()).unwrap(),
            ))
            .unwrap();
        registry
            .register_provider(Arc::new(
                AnthropicProvider::new(ProviderHttpConfig::new(anthropic_url).unwrap()).unwrap(),
            ))
            .unwrap();
        let credential = ProviderCredential::new("secret").unwrap();
        registry.refresh_models("openai", &credential).unwrap();
        registry.refresh_models("anthropic", &credential).unwrap();
        openai_thread.join().unwrap();
        anthropic_thread.join().unwrap();

        let personal = ProfileId::new();
        let work = ProfileId::new();
        registry
            .set_profile_route(
                personal.clone(),
                ModelRoute {
                    primary: selection("openai", "gpt-a", "personal-openai"),
                    fallbacks: Vec::new(),
                    classification: None,
                    summarization: None,
                    review: None,
                    vision: None,
                },
            )
            .unwrap();
        registry
            .set_profile_route(
                work.clone(),
                ModelRoute {
                    primary: selection("anthropic", "claude-a", "work-anthropic"),
                    fallbacks: Vec::new(),
                    classification: None,
                    summarization: None,
                    review: None,
                    vision: None,
                },
            )
            .unwrap();
        let personal_route = registry.resolve(&personal, ModelPurpose::Primary).unwrap();
        let work_route = registry.resolve(&work, ModelPurpose::Primary).unwrap();
        assert_eq!(personal_route.candidates[0].selection.provider, "openai");
        assert_eq!(
            personal_route.candidates[0]
                .selection
                .credential_ref
                .as_deref(),
            Some("personal-openai")
        );
        assert_eq!(work_route.candidates[0].selection.provider, "anthropic");
        assert_eq!(
            work_route.candidates[0].selection.credential_ref.as_deref(),
            Some("work-anthropic")
        );
    }

    #[test]
    fn overrides_and_specialized_routes_are_deliberate_and_validated() {
        let (openai_url, openai_thread) = discovery_server("gpt-a");
        let registry = ModelRegistry::new();
        registry
            .register_provider(Arc::new(
                OpenAiProvider::new(ProviderHttpConfig::new(openai_url).unwrap()).unwrap(),
            ))
            .unwrap();
        registry
            .refresh_models("openai", &ProviderCredential::new("credential").unwrap())
            .unwrap();
        openai_thread.join().unwrap();
        let profile = ProfileId::new();
        let primary = selection("openai", "gpt-a", "primary");
        registry
            .set_profile_route(
                profile.clone(),
                ModelRoute {
                    primary: primary.clone(),
                    fallbacks: Vec::new(),
                    classification: Some(selection("openai", "gpt-a", "classifier")),
                    summarization: None,
                    review: None,
                    vision: None,
                },
            )
            .unwrap_err();
        let mut valid_route = ModelRoute {
            primary: primary.clone(),
            fallbacks: Vec::new(),
            classification: None,
            summarization: None,
            review: None,
            vision: None,
        };
        registry
            .set_profile_route(profile.clone(), valid_route.clone())
            .unwrap();
        registry
            .set_runtime_override(&profile, selection("openai", "gpt-a", "override"))
            .unwrap();
        assert_eq!(
            registry
                .resolve(&profile, ModelPurpose::Primary)
                .unwrap()
                .candidates[0]
                .selection
                .credential_ref
                .as_deref(),
            Some("override")
        );
        registry.clear_runtime_override(&profile).unwrap();
        valid_route.classification = None;
        assert_eq!(
            registry
                .resolve(&profile, ModelPurpose::Primary)
                .unwrap()
                .candidates[0]
                .selection,
            primary
        );
    }

    #[test]
    fn transient_failure_uses_real_fallback_but_authentication_does_not() {
        let anthropic_stream = concat!(
            "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":2}}}\n\n",
            "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"fallback\"}}\n\n",
            "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"
        );
        let (openai_url, openai_thread) = response_server(vec![
            json_response(r#"{"data":[{"id":"gpt-a"}]}"#),
            http_response("503 Unavailable", "application/json", "{}"),
        ]);
        let (anthropic_url, anthropic_thread) = response_server(vec![
            json_response(r#"{"data":[{"id":"claude-a"}]}"#),
            http_response("200 OK", "text/event-stream", anthropic_stream),
        ]);
        let registry = ModelRegistry::new();
        registry
            .register_provider(Arc::new(
                OpenAiProvider::new(ProviderHttpConfig::new(openai_url).unwrap()).unwrap(),
            ))
            .unwrap();
        registry
            .register_provider(Arc::new(
                AnthropicProvider::new(ProviderHttpConfig::new(anthropic_url).unwrap()).unwrap(),
            ))
            .unwrap();
        let discovery_credential = ProviderCredential::new("discovery").unwrap();
        registry
            .refresh_models("openai", &discovery_credential)
            .unwrap();
        registry
            .refresh_models("anthropic", &discovery_credential)
            .unwrap();
        let profile = ProfileId::new();
        registry
            .set_profile_route(
                profile.clone(),
                ModelRoute {
                    primary: selection("openai", "gpt-a", "openai-key"),
                    fallbacks: vec![selection("anthropic", "claude-a", "anthropic-key")],
                    classification: None,
                    summarization: None,
                    review: None,
                    vision: None,
                },
            )
            .unwrap();
        let credentials = |provider: &str, credential_ref: Option<&str>| {
            let expected = format!("{provider}-key");
            if credential_ref == Some(expected.as_str()) {
                ProviderCredential::new(format!("{provider}-secret"))
            } else {
                Err(ProviderError::new(
                    ProviderErrorKind::Authentication,
                    "credential reference did not match provider",
                ))
            }
        };
        let mut text = String::new();
        let mut sink = |event| {
            if let ModelEvent::TextDelta { text: delta } = event {
                text.push_str(&delta);
            }
            Ok(StreamControl::Continue)
        };
        let attempt = registry
            .stream_with_fallback(
                &profile,
                ModelPurpose::Primary,
                &request(),
                &credentials,
                &CancellationToken::default(),
                &mut sink,
            )
            .unwrap();
        assert_eq!(attempt.provider, "anthropic");
        assert_eq!(attempt.fallback_index, 1);
        assert_eq!(text, "fallback");
        openai_thread.join().unwrap();
        anthropic_thread.join().unwrap();

        let terminal = ProviderError::new(ProviderErrorKind::Authentication, "invalid key");
        assert!(!terminal.allows_retry_or_fallback());
    }
}
