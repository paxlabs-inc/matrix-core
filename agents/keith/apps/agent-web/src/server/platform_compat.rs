use std::convert::Infallible;
use std::sync::Arc;
use std::time::Duration;

use axum::Json;
use axum::body::Bytes;
use axum::extract::rejection::BytesRejection;
use axum::extract::{Path, Query, State};
use axum::http::{HeaderMap, HeaderValue, StatusCode, header};
use axum::response::sse::{Event, KeepAlive, Sse};
use axum::response::{IntoResponse, Response};
use futures_util::stream;
use keith_agent_types::{ProfileId, SessionId};
use keith_protocol::{ClientCommand, WireMessage};
use ring::hmac;
use serde::Deserialize;
use serde_json::json;
use tokio::sync::{OwnedSemaphorePermit, Semaphore, mpsc};

use super::{AppState, BridgeError};

const AUTH_COMPARISON_KEY: &[u8] = b"keith-platform-compatibility-auth-v1";
const MAX_PLATFORM_COMMAND_BYTES: usize = 128 * 1024;

pub struct PlatformCompatibilityConfig {
    pub api_key: Vec<u8>,
    pub allow_non_loopback: bool,
    pub max_in_flight: usize,
}

impl std::fmt::Debug for PlatformCompatibilityConfig {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("PlatformCompatibilityConfig")
            .field("api_key", &"[REDACTED]")
            .field("allow_non_loopback", &self.allow_non_loopback)
            .field("max_in_flight", &self.max_in_flight)
            .finish()
    }
}

pub(super) struct PlatformCompatibility {
    api_key_tag: [u8; 32],
    capacity: Arc<Semaphore>,
}

struct PlatformFailure {
    status: StatusCode,
    code: &'static str,
    message: &'static str,
}

impl PlatformFailure {
    const fn new(status: StatusCode, code: &'static str, message: &'static str) -> Self {
        Self {
            status,
            code,
            message,
        }
    }

    fn into_response(self) -> Response {
        let mut response = error_response(self.status, self.code, self.message);
        if self.status == StatusCode::UNAUTHORIZED {
            response.headers_mut().insert(
                header::WWW_AUTHENTICATE,
                HeaderValue::from_static("Bearer realm=\"keith-platform\""),
            );
        }
        response
    }
}

impl PlatformCompatibility {
    pub(super) fn new(mut config: PlatformCompatibilityConfig) -> Result<Self, String> {
        if config.api_key.len() < 32 {
            config.api_key.fill(0);
            return Err("platform API key must contain at least 32 bytes".to_owned());
        }
        if config.max_in_flight == 0 {
            config.api_key.fill(0);
            return Err("platform API concurrency must be non-zero".to_owned());
        }
        let tag = hmac::sign(&comparison_key(), &config.api_key);
        let mut api_key_tag = [0_u8; 32];
        api_key_tag.copy_from_slice(tag.as_ref());
        config.api_key.fill(0);
        Ok(Self {
            api_key_tag,
            capacity: Arc::new(Semaphore::new(config.max_in_flight)),
        })
    }

    fn authorize(&self, headers: &HeaderMap) -> Result<(), PlatformFailure> {
        let supplied = bearer_token(headers).ok_or_else(authentication_response)?;
        hmac::verify(&comparison_key(), supplied, &self.api_key_tag)
            .map_err(|_| authentication_response())
    }

    fn admit(&self) -> Result<OwnedSemaphorePermit, PlatformFailure> {
        Arc::clone(&self.capacity).try_acquire_owned().map_err(|_| {
            PlatformFailure::new(
                StatusCode::TOO_MANY_REQUESTS,
                "keith_capacity_exhausted",
                "Keith platform capacity is exhausted",
            )
        })
    }
}

#[derive(Deserialize)]
struct PlatformCommand {
    session_id: Option<SessionId>,
    command: ClientCommand,
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

fn configured(
    state: &AppState,
    headers: &HeaderMap,
) -> Result<Arc<PlatformCompatibility>, PlatformFailure> {
    let compatibility = state.platform_compatibility.clone().ok_or_else(|| {
        PlatformFailure::new(
            StatusCode::NOT_FOUND,
            "platform_api_disabled",
            "Keith platform integration is not enabled",
        )
    })?;
    compatibility.authorize(headers)?;
    Ok(compatibility)
}

pub(super) async fn health(State(state): State<AppState>, headers: HeaderMap) -> Response {
    if let Err(error) = configured(&state, &headers) {
        return error.into_response();
    }
    let bridge = state.bridge.clone();
    match tokio::task::spawn_blocking(move || bridge.catalog()).await {
        Ok(Ok((profiles, _))) => Json(json!({
            "status": "ok",
            "runtime": "keith",
            "interface": "native",
            "profiles": profiles.into_iter().filter(|profile| profile.enabled).count(),
        }))
        .into_response(),
        _ => error_response(
            StatusCode::SERVICE_UNAVAILABLE,
            "keith_unavailable",
            "Keith native runtime is unavailable",
        ),
    }
}

pub(super) async fn catalog(State(state): State<AppState>, headers: HeaderMap) -> Response {
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
        bridge.catalog()
    })
    .await;
    match result {
        Ok(Ok((profiles, sessions))) => {
            if let Ok(mut cached) = state.catalog_cache.lock() {
                *cached = Some((profiles.clone(), sessions.clone()));
            }
            Json(json!({
                "profiles": profiles,
                "sessions": sessions,
            }))
            .into_response()
        }
        _ => match state
            .catalog_cache
            .lock()
            .ok()
            .and_then(|cached| cached.clone())
        {
            Some((profiles, sessions)) => Json(json!({
                "profiles": profiles,
                "sessions": sessions,
            }))
            .into_response(),
            None => error_response(
                StatusCode::SERVICE_UNAVAILABLE,
                "keith_unavailable",
                "Keith native runtime is unavailable",
            ),
        },
    }
}

pub(super) async fn capabilities(State(state): State<AppState>, headers: HeaderMap) -> Response {
    let compatibility = match configured(&state, &headers) {
        Ok(compatibility) => compatibility,
        Err(error) => return error.into_response(),
    };
    let permit = match compatibility.admit() {
        Ok(permit) => permit,
        Err(error) => return error.into_response(),
    };
    let bridge = state.bridge.clone();
    match tokio::task::spawn_blocking(move || {
        let _permit = permit;
        bridge.capabilities()
    })
    .await
    {
        Ok(Ok(features)) => Json(json!({
            "runtime": "keith",
            "interface": "native",
            "features": features,
        }))
        .into_response(),
        _ => error_response(
            StatusCode::SERVICE_UNAVAILABLE,
            "keith_unavailable",
            "Keith native runtime is unavailable",
        ),
    }
}

pub(super) async fn command(
    State(state): State<AppState>,
    Path(profile): Path<String>,
    headers: HeaderMap,
    body: Result<Bytes, BytesRejection>,
) -> Response {
    let compatibility = match configured(&state, &headers) {
        Ok(compatibility) => compatibility,
        Err(error) => return error.into_response(),
    };
    let permit = match compatibility.admit() {
        Ok(permit) => permit,
        Err(error) => return error.into_response(),
    };
    let body = match body {
        Ok(body) if body.len() <= MAX_PLATFORM_COMMAND_BYTES => body,
        Ok(_) | Err(_) => {
            return error_response(
                StatusCode::PAYLOAD_TOO_LARGE,
                "payload_too_large",
                "Keith platform command is too large",
            );
        }
    };
    let profile: ProfileId = match profile.parse() {
        Ok(profile) => profile,
        Err(_) => {
            return error_response(
                StatusCode::BAD_REQUEST,
                "invalid_profile",
                "Keith profile is invalid",
            );
        }
    };
    let request: PlatformCommand = match serde_json::from_slice(&body) {
        Ok(request) => request,
        Err(_) => {
            return error_response(
                StatusCode::BAD_REQUEST,
                "invalid_command",
                "Keith native command is invalid",
            );
        }
    };
    let bridge = state.bridge.clone();
    if headers
        .get(header::ACCEPT)
        .and_then(|value| value.to_str().ok())
        .is_some_and(|value| {
            value
                .split(',')
                .any(|media_type| media_type.trim().starts_with("text/event-stream"))
        })
    {
        return stream_command(bridge, profile, request, permit);
    }
    match tokio::task::spawn_blocking(move || {
        let _permit = permit;
        let mut client = bridge.connect()?;
        let envelope = client.envelope(request.session_id, request.command);
        super::validate_command_scope(&mut client, &profile, &envelope)?;
        client.execute(envelope)
    })
    .await
    {
        Ok(Ok(result)) => Json(result).into_response(),
        Ok(Err(BridgeError::Scope)) => error_response(
            StatusCode::FORBIDDEN,
            "scope_denied",
            "Keith command scope was denied",
        ),
        _ => error_response(
            StatusCode::SERVICE_UNAVAILABLE,
            "keith_unavailable",
            "Keith native runtime is unavailable",
        ),
    }
}

fn stream_command(
    bridge: super::DaemonBridge,
    profile: ProfileId,
    request: PlatformCommand,
    permit: OwnedSemaphorePermit,
) -> Response {
    let (sender, receiver) = mpsc::channel::<String>(super::EVENT_QUEUE_CAPACITY);
    tokio::task::spawn_blocking(move || {
        let _permit = permit;
        let result = (|| {
            let mut client = bridge.connect()?;
            let envelope = client.envelope(request.session_id, request.command);
            super::validate_command_scope(&mut client, &profile, &envelope)?;
            let stream_sender = sender.clone();
            let result = client.execute_streaming(envelope, &mut |message| {
                if let Ok(encoded) = serde_json::to_string(&message) {
                    let _ = stream_sender.blocking_send(encoded);
                }
            })?;
            let encoded = serde_json::to_string(&WireMessage::CommandResult(result))?;
            sender
                .blocking_send(encoded)
                .map_err(|_| BridgeError::Response)
        })();
        let _ = result;
    });
    let events = stream::unfold(receiver, |mut receiver| async move {
        receiver.recv().await.map(|payload| {
            (
                Ok::<Event, Infallible>(Event::default().data(payload)),
                receiver,
            )
        })
    });
    let mut response = Sse::new(events)
        .keep_alive(
            KeepAlive::new()
                .interval(Duration::from_secs(15))
                .text("keep-alive"),
        )
        .into_response();
    response.headers_mut().insert(
        header::CACHE_CONTROL,
        HeaderValue::from_static("no-store, no-transform"),
    );
    response.headers_mut().insert(
        header::X_CONTENT_TYPE_OPTIONS,
        HeaderValue::from_static("nosniff"),
    );
    response
}

pub(super) async fn events(
    State(state): State<AppState>,
    Path((profile, session)): Path<(String, String)>,
    Query(resume): Query<super::ResumeQuery>,
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
    let profile: ProfileId = match profile.parse() {
        Ok(profile) => profile,
        Err(_) => {
            return error_response(
                StatusCode::BAD_REQUEST,
                "invalid_profile",
                "Keith profile is invalid",
            );
        }
    };
    let session: SessionId = match session.parse() {
        Ok(session) => session,
        Err(_) => {
            return error_response(
                StatusCode::BAD_REQUEST,
                "invalid_session",
                "Keith session is invalid",
            );
        }
    };
    let bridge = state.bridge.clone();
    let checked_bridge = bridge.clone();
    let checked_profile = profile.clone();
    let checked_session = session.clone();
    let scoped = tokio::task::spawn_blocking(move || {
        checked_bridge.session_in_profile(&checked_profile, &checked_session)
    })
    .await;
    if !matches!(scoped, Ok(Ok(true))) {
        return error_response(
            StatusCode::FORBIDDEN,
            "scope_denied",
            "Keith event scope was denied",
        );
    }

    let (sender, receiver) = mpsc::channel::<String>(super::EVENT_QUEUE_CAPACITY);
    tokio::task::spawn_blocking(move || {
        let _permit = permit;
        let _ = bridge.subscribe(&profile, &session, resume, &sender);
    });
    let events = stream::unfold(receiver, |mut receiver| async move {
        receiver.recv().await.map(|payload| {
            (
                Ok::<Event, Infallible>(Event::default().data(payload)),
                receiver,
            )
        })
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

fn authentication_response() -> PlatformFailure {
    PlatformFailure::new(
        StatusCode::UNAUTHORIZED,
        "authentication_error",
        "Keith platform authentication failed",
    )
}

fn error_response(status: StatusCode, code: &str, message: &str) -> Response {
    (
        status,
        Json(json!({"error": {"code": code, "message": message}})),
    )
        .into_response()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn platform_key_is_redacted_and_checked_in_constant_time() {
        let compatibility = PlatformCompatibility::new(PlatformCompatibilityConfig {
            api_key: b"platform-compatibility-secret-key".to_vec(),
            allow_non_loopback: false,
            max_in_flight: 2,
        })
        .unwrap();
        let mut headers = HeaderMap::new();
        headers.insert(
            header::AUTHORIZATION,
            HeaderValue::from_static("Bearer platform-compatibility-secret-key"),
        );
        assert!(compatibility.authorize(&headers).is_ok());
        headers.insert(
            header::AUTHORIZATION,
            HeaderValue::from_static("Bearer platform-compatibility-secret-kex"),
        );
        assert!(compatibility.authorize(&headers).is_err());
    }

    #[test]
    fn platform_key_and_capacity_fail_closed() {
        assert!(
            PlatformCompatibility::new(PlatformCompatibilityConfig {
                api_key: b"short".to_vec(),
                allow_non_loopback: false,
                max_in_flight: 1,
            })
            .is_err()
        );
        assert!(
            PlatformCompatibility::new(PlatformCompatibilityConfig {
                api_key: vec![b'x'; 32],
                allow_non_loopback: false,
                max_in_flight: 0,
            })
            .is_err()
        );
    }
}
