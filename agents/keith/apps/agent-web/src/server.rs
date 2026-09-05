use std::collections::BTreeSet;
use std::convert::Infallible;
use std::ffi::OsString;
use std::fs::OpenOptions;
use std::io::Write as _;
use std::net::SocketAddr;
use std::path::{Path as FsPath, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use axum::body::Bytes;
use axum::extract::ws::{Message, WebSocket, WebSocketUpgrade};
use axum::extract::{DefaultBodyLimit, Form, Path, Query, State};
use axum::http::{HeaderMap, HeaderValue, StatusCode, header};
use axum::response::sse::{Event, KeepAlive, Sse};
use axum::response::{IntoResponse, Redirect, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use futures_util::{SinkExt, StreamExt, stream};
use keith_agent_types::{
    CURRENT_PROTOCOL_VERSION, ClientId, CommandId, EntityId, ProfileId, Sequence, SessionId,
    UtcTimestamp,
};
use keith_connection::{
    AgentTransport, FramedTransport, LocalStream, connect_local, set_local_read_timeout,
    set_local_write_timeout,
};
use keith_credentials::{
    BrowserWritePolicy, CredentialOwner, CredentialRef, CsrfToken, EncryptedCredentialStore,
    MasterKey, NativeMasterKeyStore, RestrictedMasterKeyStore, SecretValue,
};
use keith_framing::FrameError;
use keith_platform::PlatformPaths;
use keith_protocol::{
    AttachSession, ClientCommand, ClientHello, CommandEnvelope, CommandResult,
    CommandResultEnvelope, EvolutionCommand, Feature, ProfileSummary, ResponsePayload,
    ResumeCursor, SessionFilter, SessionSummary, WireFormat, WireMessage,
};
use keith_provider_catalog::provider as provider_spec;
use serde::Deserialize;
use sha2::{Digest, Sha256};
use thiserror::Error;
use tokio::net::TcpListener;
use tokio::sync::mpsc;
use url::Url;

use crate::security::{BrowserSecurity, SecurityError};
mod openai_compat;
mod platform_compat;

pub use openai_compat::OpenAiCompatibilityConfig;
pub use platform_compat::PlatformCompatibilityConfig;

const MAX_BROWSER_BODY_BYTES: usize = 128 * 1024;
const MAX_ATTACHMENT_BYTES: usize = 25 * 1_024 * 1_024;
const EVENT_QUEUE_CAPACITY: usize = 256;
const UI_INDEX: &str = "ui/index.html";

pub struct WebServerConfig {
    pub bind: SocketAddr,
    pub exact_origin: String,
    pub daemon_socket: PathBuf,
    pub asset_root: PathBuf,
    pub credential_root: PathBuf,
    pub credential_key: MasterKey,
    pub login_secret: Vec<u8>,
    pub session_lifetime: Duration,
    pub mutation_limit_per_second: usize,
    pub daemon_timeout: Duration,
    pub openai_compatibility: Option<OpenAiCompatibilityConfig>,
    pub platform_compatibility: Option<PlatformCompatibilityConfig>,
}

impl std::fmt::Debug for WebServerConfig {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("WebServerConfig")
            .field("bind", &self.bind)
            .field("exact_origin", &self.exact_origin)
            .field("daemon_socket", &self.daemon_socket)
            .field("asset_root", &self.asset_root)
            .field("credential_root", &self.credential_root)
            .field("credential_key", &"[REDACTED]")
            .field("login_secret", &"[REDACTED]")
            .field("session_lifetime", &self.session_lifetime)
            .field("mutation_limit_per_second", &self.mutation_limit_per_second)
            .field("daemon_timeout", &self.daemon_timeout)
            .field(
                "openai_compatibility",
                &self
                    .openai_compatibility
                    .as_ref()
                    .map(|_| "[CONFIGURED, SECRET REDACTED]"),
            )
            .field(
                "platform_compatibility",
                &self
                    .platform_compatibility
                    .as_ref()
                    .map(|_| "[CONFIGURED, SECRET REDACTED]"),
            )
            .finish()
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum CredentialKeySource {
    Environment(String),
    Native { service: String, account: String },
    Restricted(PathBuf),
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ServerArguments {
    pub bind: SocketAddr,
    pub exact_origin: String,
    pub daemon_socket: PathBuf,
    pub asset_root: PathBuf,
    pub credential_root: PathBuf,
    pub login_secret_env: String,
    pub credential_key_source: CredentialKeySource,
    pub openai_api_key_env: String,
    pub openai_allow_non_loopback: bool,
}

#[derive(Debug, Error)]
pub enum ServerError {
    #[error("server configuration is invalid: {0}")]
    Configuration(String),
    #[error("authentication setup failed")]
    Security(#[from] SecurityError),
    #[error("credential storage setup failed")]
    Credentials(#[from] keith_credentials::CredentialError),
    #[error("server I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("HTTP server failed: {0}")]
    Http(#[from] axum::Error),
}

#[derive(Clone)]
struct AppState {
    security: Arc<BrowserSecurity>,
    bridge: DaemonBridge,
    credential_store: Arc<EncryptedCredentialStore>,
    exact_origin: String,
    asset_root: PathBuf,
    openai_compatibility: Option<Arc<openai_compat::OpenAiCompatibility>>,
    platform_compatibility: Option<Arc<platform_compat::PlatformCompatibility>>,
    catalog_cache: CatalogCache,
    attachment_root: PathBuf,
}

type CatalogCache = Arc<Mutex<Option<(Vec<ProfileSummary>, Vec<SessionSummary>)>>>;

pub struct WebServer {
    state: AppState,
    bind: SocketAddr,
}

impl WebServer {
    /// Opens the authenticated boundary and encrypted credential store.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid configuration, authentication setup, or credential storage.
    pub fn new(mut config: WebServerConfig) -> Result<Self, ServerError> {
        validate_config(&config)?;
        let security = BrowserSecurity::new(
            config.exact_origin.clone(),
            &config.login_secret,
            config.session_lifetime,
            config.mutation_limit_per_second,
        )?;
        config.login_secret.fill(0);
        let openai_compatibility = config
            .openai_compatibility
            .take()
            .map(openai_compat::OpenAiCompatibility::new)
            .transpose()
            .map_err(ServerError::Configuration)?
            .map(Arc::new);
        let platform_compatibility = config
            .platform_compatibility
            .take()
            .map(platform_compat::PlatformCompatibility::new)
            .transpose()
            .map_err(ServerError::Configuration)?
            .map(Arc::new);
        let credential_store =
            EncryptedCredentialStore::open(&config.credential_root, config.credential_key)?;
        let attachment_root = config
            .daemon_socket
            .parent()
            .ok_or_else(|| ServerError::Configuration("daemon socket has no data root".into()))?
            .join("channel-staging")
            .join("inbound");
        Ok(Self {
            state: AppState {
                security: Arc::new(security),
                bridge: DaemonBridge {
                    socket_path: config.daemon_socket,
                    timeout: config.daemon_timeout,
                },
                credential_store: Arc::new(credential_store),
                exact_origin: config.exact_origin,
                asset_root: config.asset_root,
                openai_compatibility,
                platform_compatibility,
                catalog_cache: Arc::new(Mutex::new(None)),
                attachment_root,
            },
            bind: config.bind,
        })
    }

    pub fn router(&self) -> Router {
        Router::new()
            .route("/", get(app))
            .route("/login", get(login))
            .route("/favicon.ico", get(favicon))
            .route("/auth/session", post(create_session))
            .route("/api/bootstrap", get(bootstrap))
            .route("/api/evolution/commands", post(evolution_command))
            .route("/assets/ui/{*path}", get(ui_asset))
            .route("/api/profiles/{profile}/commands", post(command))
            .route(
                "/api/profiles/{profile}/sessions/{session}/attachments",
                post(upload_attachment).layer(DefaultBodyLimit::max(MAX_ATTACHMENT_BYTES)),
            )
            .route(
                "/api/profiles/{profile}/credentials",
                post(write_credential),
            )
            .route("/api/events/{profile}/{session}", get(events))
            .route("/v1/models", get(openai_compat::models))
            .route("/v1/models/{model}", get(openai_compat::model))
            .route(
                "/v1/chat/completions",
                post(openai_compat::chat_completions),
            )
            .route("/platform/v1/health", get(platform_compat::health))
            .route("/platform/v1/catalog", get(platform_compat::catalog))
            .route(
                "/platform/v1/capabilities",
                get(platform_compat::capabilities),
            )
            .route(
                "/platform/v1/profiles/{profile}/commands",
                post(platform_compat::command),
            )
            .route(
                "/platform/v1/events/{profile}/{session}",
                get(platform_compat::events),
            )
            .layer(DefaultBodyLimit::max(MAX_BROWSER_BODY_BYTES))
            .with_state(self.state.clone())
    }

    /// Runs the HTTP server until an operating-system shutdown signal arrives.
    ///
    /// # Errors
    ///
    /// Returns an error when the listener or HTTP server fails.
    pub async fn run(self) -> Result<(), ServerError> {
        let listener = TcpListener::bind(self.bind).await?;
        axum::serve(listener, self.router())
            .with_graceful_shutdown(shutdown_signal())
            .await?;
        Ok(())
    }

    /// Runs the HTTP server on an already-bound listener.
    ///
    /// # Errors
    ///
    /// Returns an error when the HTTP server fails.
    pub async fn serve_listener(self, listener: TcpListener) -> Result<(), ServerError> {
        axum::serve(listener, self.router()).await?;
        Ok(())
    }
}

async fn favicon() -> Response {
    StatusCode::NO_CONTENT.into_response()
}

impl ServerArguments {
    /// Parses server arguments without accepting secret values on the command line.
    ///
    /// # Errors
    ///
    /// Returns a safe error for missing, malformed, or unknown arguments.
    pub fn parse<I, S>(arguments: I) -> Result<Option<Self>, String>
    where
        I: IntoIterator<Item = S>,
        S: Into<OsString>,
    {
        let mut arguments = arguments.into_iter().map(Into::into);
        let _program = arguments.next();
        let mut bind = SocketAddr::from(([127, 0, 0, 1], 7341));
        let mut exact_origin = "http://127.0.0.1:7341".to_owned();
        let mut daemon_socket = None;
        let mut asset_root = PathBuf::from("apps/agent-web/static");
        let mut credential_root = None;
        let mut login_secret_env = "KEITH_WEB_LOGIN_SECRET".to_owned();
        let mut openai_api_key_env = "KEITH_OPENAI_COMPAT_API_KEY".to_owned();
        let mut openai_allow_non_loopback = false;
        let mut credential_key_source = None;
        while let Some(argument) = arguments.next() {
            let argument = argument
                .into_string()
                .map_err(|_| "arguments must be UTF-8".to_owned())?;
            if matches!(argument.as_str(), "--version" | "-V") {
                println!("agent-web {}", env!("CARGO_PKG_VERSION"));
                return Ok(None);
            }
            let value = arguments
                .next()
                .ok_or_else(|| format!("missing value for {argument}"))?;
            match argument.as_str() {
                "--bind" => {
                    bind = value
                        .to_string_lossy()
                        .parse()
                        .map_err(|_| "invalid bind address".to_owned())?;
                }
                "--origin" => {
                    exact_origin = value
                        .into_string()
                        .map_err(|_| "origin must be UTF-8".to_owned())?;
                }
                "--socket" => daemon_socket = Some(PathBuf::from(value)),
                "--asset-root" => asset_root = PathBuf::from(value),
                "--credential-root" => credential_root = Some(PathBuf::from(value)),
                "--login-secret-env" => {
                    login_secret_env = value
                        .into_string()
                        .map_err(|_| "environment name must be UTF-8".to_owned())?;
                }
                "--credential-key-env" => {
                    credential_key_source = Some(CredentialKeySource::Environment(
                        value
                            .into_string()
                            .map_err(|_| "environment name must be UTF-8".to_owned())?,
                    ));
                }
                "--credential-key-native-account" => {
                    credential_key_source = Some(CredentialKeySource::Native {
                        service: "keith-agent".to_owned(),
                        account: value
                            .into_string()
                            .map_err(|_| "native account must be UTF-8".to_owned())?,
                    });
                }
                "--openai-api-key-env" => {
                    openai_api_key_env = value
                        .into_string()
                        .map_err(|_| "environment name must be UTF-8".to_owned())?;
                }
                "--openai-allow-non-loopback" => {
                    openai_allow_non_loopback = parse_boolean(&value.to_string_lossy())?;
                }
                _ => return Err(format!("unknown argument {argument}")),
            }
        }
        let platform_paths = if daemon_socket.is_none() || credential_root.is_none() {
            Some(PlatformPaths::discover().map_err(|error| error.to_string())?)
        } else {
            None
        };
        let daemon_socket = daemon_socket.or_else(|| {
            platform_paths
                .as_ref()
                .map(|paths| paths.daemon_endpoint.clone())
        });
        let credential_root = credential_root.or_else(|| {
            platform_paths
                .as_ref()
                .map(|paths| paths.credential_root.clone())
        });
        let credential_root =
            credential_root.ok_or_else(|| "native credential root is unavailable".to_owned())?;
        let credential_key_source = credential_key_source
            .unwrap_or_else(|| CredentialKeySource::Restricted(credential_root.clone()));
        Ok(Some(Self {
            bind,
            exact_origin,
            daemon_socket: daemon_socket
                .ok_or_else(|| "native daemon endpoint is unavailable".to_owned())?,
            asset_root,
            credential_root,
            login_secret_env,
            credential_key_source,
            openai_api_key_env,
            openai_allow_non_loopback,
        }))
    }

    /// Resolves the named secret environment variables into a redacted server configuration.
    ///
    /// # Errors
    ///
    /// Returns a safe error when an environment variable is missing or the key is malformed.
    pub fn load_config(self) -> Result<WebServerConfig, String> {
        let login_secret = std::env::var_os(&self.login_secret_env)
            .ok_or_else(|| format!("{} is unavailable", self.login_secret_env))?
            .into_encoded_bytes();
        let credential_key = match self.credential_key_source {
            CredentialKeySource::Environment(environment) => {
                let mut encoded_key = std::env::var_os(&environment)
                    .ok_or_else(|| format!("{environment} is unavailable"))?
                    .into_encoded_bytes();
                let decoded_key = decode_key(&encoded_key)?;
                encoded_key.fill(0);
                MasterKey::from_bytes(decoded_key)
            }
            CredentialKeySource::Native { service, account } => {
                NativeMasterKeyStore::new(service, account)
                    .and_then(|store| store.load_or_create())
                    .map_err(|error| error.to_string())?
            }
            CredentialKeySource::Restricted(root) => RestrictedMasterKeyStore::open(root)
                .and_then(|store| store.load_or_create())
                .map_err(|error| error.to_string())?,
        };
        let openai_compatibility =
            std::env::var_os(&self.openai_api_key_env).map(|value| OpenAiCompatibilityConfig {
                api_key: value.into_encoded_bytes(),
                allow_non_loopback: self.openai_allow_non_loopback,
                max_in_flight: 16,
            });
        let platform_allow_non_loopback = std::env::var_os("KEITH_PLATFORM_ALLOW_NON_LOOPBACK")
            .map(|value| parse_boolean(&value.to_string_lossy()))
            .transpose()?
            .unwrap_or(false);
        let platform_compatibility =
            std::env::var_os("KEITH_PLATFORM_API_KEY").map(|value| PlatformCompatibilityConfig {
                api_key: value.into_encoded_bytes(),
                allow_non_loopback: platform_allow_non_loopback,
                max_in_flight: 32,
            });
        Ok(WebServerConfig {
            bind: self.bind,
            exact_origin: self.exact_origin,
            daemon_socket: self.daemon_socket,
            asset_root: self.asset_root,
            credential_root: self.credential_root,
            credential_key,
            login_secret,
            session_lifetime: Duration::from_secs(8 * 60 * 60),
            mutation_limit_per_second: 24,
            daemon_timeout: Duration::from_secs(180),
            openai_compatibility,
            platform_compatibility,
        })
    }
}

async fn login(State(state): State<AppState>) -> Response {
    next_app_page(&state.asset_root)
}

#[derive(Deserialize)]
struct LoginForm {
    password: String,
}

async fn create_session(
    State(state): State<AppState>,
    headers: HeaderMap,
    Form(form): Form<LoginForm>,
) -> Response {
    let mut secret = form.password.into_bytes();
    let issued = state.security.issue_for_request(&headers, &secret);
    secret.fill(0);
    match issued {
        Ok(issued) => {
            let mut response = Redirect::to("/").into_response();
            if let Ok(cookie) = HeaderValue::from_str(&issued.cookie) {
                response.headers_mut().insert(header::SET_COOKIE, cookie);
            }
            response
        }
        Err(error) => security_response(error),
    }
}

#[derive(Default, Deserialize)]
struct AppSelection {
    session: Option<String>,
}

async fn app(State(state): State<AppState>, headers: HeaderMap) -> Response {
    if state.security.authenticate(&headers).is_err() {
        return Redirect::to("/login").into_response();
    }
    next_app_page(&state.asset_root)
}

async fn bootstrap(
    State(state): State<AppState>,
    Query(selection): Query<AppSelection>,
    headers: HeaderMap,
) -> Response {
    let authenticated = match state.security.authenticate(&headers) {
        Ok(authenticated) => authenticated,
        Err(error) => return security_response(error),
    };
    let csrf = match state.security.csrf(authenticated) {
        Ok(csrf) => csrf,
        Err(error) => return security_response(error),
    };
    let bridge = state.bridge.clone();
    let catalog = tokio::task::spawn_blocking(move || bridge.catalog()).await;
    match catalog {
        Ok(Ok((profiles, mut sessions))) => {
            let mut response = Json(bootstrap_payload(
                &csrf,
                &profiles,
                &mut sessions,
                selection.session.as_deref(),
            ))
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
        Ok(Err(error)) => safe_error(StatusCode::SERVICE_UNAVAILABLE, &error.to_string()),
        Err(_) => safe_error(
            StatusCode::SERVICE_UNAVAILABLE,
            "agent connection unavailable",
        ),
    }
}

/// Builds the client bootstrap projection.
///
/// Extracted from the `bootstrap` handler so the same pure path the server
/// serves can be measured directly by the performance runner.
pub fn bootstrap_payload(
    csrf: &str,
    profiles: &[ProfileSummary],
    sessions: &mut Vec<SessionSummary>,
    requested_session: Option<&str>,
) -> serde_json::Value {
    prioritize_session(sessions, requested_session);
    serde_json::json!({
        "protocol": CURRENT_PROTOCOL_VERSION,
        "csrf": csrf,
        "profiles": profiles,
        "sessions": sessions,
    })
}

fn prioritize_session(sessions: &mut Vec<SessionSummary>, requested: Option<&str>) {
    let Some(requested) = requested.and_then(|value| value.parse::<SessionId>().ok()) else {
        return;
    };
    if let Some(index) = sessions
        .iter()
        .position(|session| session.session_id == requested)
    {
        let selected = sessions.remove(index);
        sessions.insert(0, selected);
    }
}

async fn ui_asset(State(state): State<AppState>, Path(path): Path<String>) -> Response {
    let Some(path) = safe_ui_asset_path(&path) else {
        return safe_error(StatusCode::NOT_FOUND, "asset unavailable");
    };
    let media_type = match path.extension().and_then(|value| value.to_str()) {
        Some("js" | "mjs") => "text/javascript; charset=utf-8",
        Some("css") => "text/css; charset=utf-8",
        Some("json" | "map") => "application/json; charset=utf-8",
        Some("html") => "text/html; charset=utf-8",
        Some("svg") => "image/svg+xml",
        Some("png") => "image/png",
        Some("webp") => "image/webp",
        Some("ico") => "image/x-icon",
        Some("woff2") => "font/woff2",
        _ => "application/octet-stream",
    };
    file_asset(&state.asset_root.join("ui"), &path, media_type)
}

#[derive(Deserialize)]
struct AttachmentSelection {
    name: String,
}

async fn upload_attachment(
    State(state): State<AppState>,
    Path((profile, session)): Path<(String, String)>,
    Query(selection): Query<AttachmentSelection>,
    headers: HeaderMap,
    body: Bytes,
) -> Response {
    let Some(csrf) = headers
        .get("x-keith-csrf")
        .and_then(|value| value.to_str().ok())
    else {
        return security_response(SecurityError::Csrf);
    };
    if let Err(error) = state.security.authorize_mutation(&headers, csrf) {
        return security_response(error);
    }
    let profile: ProfileId = match profile.parse() {
        Ok(profile) => profile,
        Err(_) => return safe_error(StatusCode::BAD_REQUEST, "invalid profile scope"),
    };
    let session: SessionId = match session.parse() {
        Ok(session) => session,
        Err(_) => return safe_error(StatusCode::BAD_REQUEST, "invalid session scope"),
    };
    if !safe_attachment_name(&selection.name)
        || body.is_empty()
        || body.len() > MAX_ATTACHMENT_BYTES
    {
        return safe_error(
            StatusCode::BAD_REQUEST,
            "attachment name or size is invalid",
        );
    }
    let media_type = headers
        .get(header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .unwrap_or("application/octet-stream");
    if !safe_media_type(media_type) {
        return safe_error(StatusCode::BAD_REQUEST, "attachment media type is invalid");
    }
    let staging_file = EntityId::new().to_string();
    let Ok(path) = write_browser_attachment(&state.attachment_root, &staging_file, &body) else {
        return safe_error(
            StatusCode::SERVICE_UNAVAILABLE,
            "attachment staging is unavailable",
        );
    };
    let byte_length = u64::try_from(body.len()).unwrap_or(u64::MAX);
    let sha256 = hex_sha256(&body);
    let envelope = CommandEnvelope {
        protocol: CURRENT_PROTOCOL_VERSION,
        command_id: CommandId::new(),
        client_id: ClientId::new(),
        sent_at: UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH),
        session_id: Some(session.clone()),
        command: ClientCommand::StageAttachment(keith_protocol::StagedAttachment {
            session_id: session,
            staging_file,
            file_name: selection.name,
            media_type: media_type.to_owned(),
            byte_length,
            sha256,
        }),
    };
    let bridge = state.bridge.clone();
    let result =
        tokio::task::spawn_blocking(move || bridge.execute_scoped(&profile, envelope)).await;
    match result {
        Ok(Ok(result)) => match result.result {
            keith_protocol::CommandResult::Data(payload) => {
                if let ResponsePayload::Artifact(artifact_id) = *payload {
                    Json(serde_json::json!({"artifact_id": artifact_id})).into_response()
                } else {
                    let _ = std::fs::remove_file(path);
                    safe_error(
                        StatusCode::BAD_GATEWAY,
                        "Keith returned an invalid attachment response",
                    )
                }
            }
            keith_protocol::CommandResult::Rejected(error) => {
                let _ = std::fs::remove_file(path);
                safe_error(StatusCode::CONFLICT, &error.error.message)
            }
            keith_protocol::CommandResult::Accepted { .. } => {
                let _ = std::fs::remove_file(path);
                safe_error(
                    StatusCode::BAD_GATEWAY,
                    "Keith did not persist the attachment",
                )
            }
        },
        Ok(Err(BridgeError::Scope)) => {
            let _ = std::fs::remove_file(path);
            safe_error(StatusCode::FORBIDDEN, "attachment scope denied")
        }
        Ok(Err(_)) | Err(_) => {
            let _ = std::fs::remove_file(path);
            safe_error(
                StatusCode::SERVICE_UNAVAILABLE,
                "attachment upload is unavailable",
            )
        }
    }
}

fn safe_attachment_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 255
        && name != "."
        && name != ".."
        && !name.contains(['/', '\\'])
        && !name.chars().any(char::is_control)
}

fn safe_media_type(media_type: &str) -> bool {
    media_type.len() <= 255
        && media_type.split_once('/').is_some_and(|(kind, subtype)| {
            !kind.is_empty()
                && !subtype.is_empty()
                && media_type.bytes().all(|byte| {
                    byte.is_ascii_alphanumeric() || matches!(byte, b'/' | b'-' | b'+' | b'.')
                })
        })
}

fn write_browser_attachment(root: &FsPath, token: &str, body: &[u8]) -> std::io::Result<PathBuf> {
    std::fs::create_dir_all(root)?;
    let metadata = std::fs::symlink_metadata(root)?;
    if metadata.file_type().is_symlink() || !metadata.is_dir() {
        return Err(std::io::Error::other("unsafe attachment staging root"));
    }
    let path = root.join(token);
    let mut options = OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt as _;
        options.mode(0o600);
    }
    let mut file = options.open(&path)?;
    if let Err(error) = file.write_all(body).and_then(|()| file.sync_all()) {
        let _ = std::fs::remove_file(&path);
        return Err(error);
    }
    Ok(path)
}

fn hex_sha256(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

async fn command(
    State(state): State<AppState>,
    Path(profile): Path<String>,
    headers: HeaderMap,
    body: Bytes,
) -> Response {
    let Some(csrf) = headers
        .get("x-keith-csrf")
        .and_then(|value| value.to_str().ok())
    else {
        return security_response(SecurityError::Csrf);
    };
    if let Err(error) = state.security.authorize_mutation(&headers, csrf) {
        return security_response(error);
    }
    let profile: ProfileId = match profile.parse() {
        Ok(profile) => profile,
        Err(_) => return safe_error(StatusCode::BAD_REQUEST, "invalid profile scope"),
    };
    let envelope: CommandEnvelope = match serde_json::from_slice(&body) {
        Ok(envelope) => envelope,
        Err(_) => return safe_error(StatusCode::BAD_REQUEST, "invalid command envelope"),
    };
    if accepts_event_stream(&headers) {
        return stream_browser_command(state.bridge.clone(), profile, envelope);
    }
    let bridge = state.bridge.clone();
    let result =
        tokio::task::spawn_blocking(move || bridge.execute_scoped(&profile, envelope)).await;
    match result {
        Ok(Ok(result)) => Json(WireMessage::CommandResult(result)).into_response(),
        Ok(Err(BridgeError::Scope)) => safe_error(StatusCode::FORBIDDEN, "command scope denied"),
        Ok(Err(error)) => safe_error(StatusCode::SERVICE_UNAVAILABLE, &error.to_string()),
        Err(_) => safe_error(
            StatusCode::SERVICE_UNAVAILABLE,
            "agent connection unavailable",
        ),
    }
}

async fn evolution_command(
    State(state): State<AppState>,
    headers: HeaderMap,
    body: Bytes,
) -> Response {
    let Some(csrf) = headers
        .get("x-keith-csrf")
        .and_then(|value| value.to_str().ok())
    else {
        return security_response(SecurityError::Csrf);
    };
    if let Err(error) = state.security.authorize_mutation(&headers, csrf) {
        return security_response(error);
    }
    let Ok(command) = parse_evolution_command(&body) else {
        return safe_error(StatusCode::BAD_REQUEST, "invalid evolution command");
    };
    let bridge = state.bridge.clone();
    let result = tokio::task::spawn_blocking(move || bridge.execute_evolution(command)).await;
    match result {
        Ok(Ok(result)) => Json(WireMessage::CommandResult(result)).into_response(),
        Ok(Err(error)) => safe_error(StatusCode::SERVICE_UNAVAILABLE, &error.to_string()),
        Err(_) => safe_error(
            StatusCode::SERVICE_UNAVAILABLE,
            "agent connection unavailable",
        ),
    }
}

fn parse_evolution_command(body: &[u8]) -> Result<EvolutionCommand, ()> {
    let supplied: serde_json::Value = serde_json::from_slice(body).map_err(|_| ())?;
    let command: EvolutionCommand = serde_json::from_value(supplied.clone()).map_err(|_| ())?;
    let canonical = serde_json::to_value(&command).map_err(|_| ())?;
    (canonical == supplied).then_some(command).ok_or(())
}

fn accepts_event_stream(headers: &HeaderMap) -> bool {
    headers
        .get(header::ACCEPT)
        .and_then(|value| value.to_str().ok())
        .is_some_and(|value| {
            value
                .split(',')
                .any(|media_type| media_type.trim().starts_with("text/event-stream"))
        })
}

fn stream_browser_command(
    bridge: DaemonBridge,
    profile: ProfileId,
    envelope: CommandEnvelope,
) -> Response {
    let (sender, receiver) = mpsc::channel::<String>(EVENT_QUEUE_CAPACITY);
    tokio::task::spawn_blocking(move || {
        let stream_sender = sender.clone();
        let result = bridge.execute_scoped_streaming(&profile, envelope, &mut |message| {
            if let Ok(encoded) = serde_json::to_string(&message) {
                let _ = stream_sender.blocking_send(encoded);
            }
        });
        match result {
            Ok(result) => {
                if let Ok(encoded) = serde_json::to_string(&WireMessage::CommandResult(result)) {
                    let _ = sender.blocking_send(encoded);
                }
            }
            Err(error) => {
                let safe_message = if matches!(error, BridgeError::Scope) {
                    "command scope denied"
                } else {
                    "Keith's native event stream became unavailable"
                };
                let encoded = serde_json::json!({
                    "message": "stream_error",
                    "payload": { "safe_message": safe_message }
                })
                .to_string();
                let _ = sender.blocking_send(encoded);
            }
        }
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

#[derive(Deserialize)]
struct CredentialForm {
    csrf: String,
    provider: String,
    name: String,
    secret: String,
}

async fn write_credential(
    State(state): State<AppState>,
    Path(profile): Path<String>,
    headers: HeaderMap,
    Form(form): Form<CredentialForm>,
) -> Response {
    if let Err(error) = state.security.authorize_mutation(&headers, &form.csrf) {
        return security_response(error);
    }
    let profile: ProfileId = match profile.parse() {
        Ok(profile) => profile,
        Err(_) => return safe_error(StatusCode::BAD_REQUEST, "invalid profile scope"),
    };
    let bridge = state.bridge.clone();
    let scoped = tokio::task::spawn_blocking(move || bridge.profile_exists(&profile)).await;
    if !matches!(scoped, Ok(Ok(true))) {
        return safe_error(StatusCode::FORBIDDEN, "credential scope denied");
    }
    if provider_spec(&form.provider).is_none() {
        return safe_error(StatusCode::BAD_REQUEST, "credential provider is invalid");
    }
    let origin = headers
        .get(header::ORIGIN)
        .and_then(|value| value.to_str().ok())
        .unwrap_or_default();
    let policy = match CsrfToken::new(form.csrf.as_bytes()) {
        Ok(csrf) => BrowserWritePolicy {
            exact_origin: state.exact_origin.clone(),
            csrf,
            max_payload_bytes: MAX_BROWSER_BODY_BYTES,
        },
        Err(_) => return safe_error(StatusCode::BAD_REQUEST, "credential request denied"),
    };
    let Ok(reference) = CredentialRef::new(form.name, CredentialOwner::Provider(form.provider))
    else {
        return safe_error(StatusCode::BAD_REQUEST, "credential reference is invalid");
    };
    let content_length = form
        .secret
        .len()
        .saturating_add(reference.name.len())
        .saturating_add(form.csrf.len());
    let Ok(secret) = SecretValue::new(form.secret.into_bytes()) else {
        return safe_error(StatusCode::BAD_REQUEST, "credential value is invalid");
    };
    match state.credential_store.configure_from_browser(
        &policy,
        true,
        origin,
        form.csrf.as_bytes(),
        reference,
        secret,
        content_length,
        UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH),
    ) {
        Ok(_) => Redirect::to("/?credential=configured").into_response(),
        Err(_) => safe_error(StatusCode::BAD_REQUEST, "credential request denied"),
    }
}

#[derive(Clone, Copy, Debug, Default, Deserialize)]
struct ResumeQuery {
    generation: Option<u64>,
    sequence: Option<u64>,
}

async fn events(
    websocket: WebSocketUpgrade,
    State(state): State<AppState>,
    Path((profile, session)): Path<(String, String)>,
    Query(resume): Query<ResumeQuery>,
    headers: HeaderMap,
) -> Response {
    if let Err(error) = state.security.authorize_read_socket(&headers) {
        return security_response(error);
    }
    let profile: ProfileId = match profile.parse() {
        Ok(profile) => profile,
        Err(_) => return safe_error(StatusCode::BAD_REQUEST, "invalid profile scope"),
    };
    let session: SessionId = match session.parse() {
        Ok(session) => session,
        Err(_) => return safe_error(StatusCode::BAD_REQUEST, "invalid session scope"),
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
        return safe_error(StatusCode::FORBIDDEN, "subscription scope denied");
    }
    websocket
        .max_message_size(MAX_BROWSER_BODY_BYTES)
        .on_upgrade(move |socket| subscription_socket(socket, bridge, profile, session, resume))
}

async fn subscription_socket(
    socket: WebSocket,
    bridge: DaemonBridge,
    profile: ProfileId,
    session: SessionId,
    resume: ResumeQuery,
) {
    let (sender, mut receiver) = mpsc::channel(EVENT_QUEUE_CAPACITY);
    tokio::task::spawn_blocking(move || {
        let _ = bridge.subscribe(&profile, &session, resume, &sender);
    });
    let (mut websocket_sender, mut websocket_receiver) = socket.split();
    loop {
        tokio::select! {
            outbound = receiver.recv() => {
                let Some(outbound) = outbound else { break; };
                if websocket_sender.send(Message::Text(outbound.into())).await.is_err() {
                    break;
                }
            }
            inbound = websocket_receiver.next() => {
                match inbound {
                    Some(Ok(Message::Close(_)) | Err(_)) | None => break,
                    Some(Ok(Message::Ping(value))) => {
                        if websocket_sender.send(Message::Pong(value)).await.is_err() { break; }
                    }
                    Some(Ok(_)) => {
                        let _ = websocket_sender.send(Message::Close(None)).await;
                        break;
                    }
                }
            }
        }
    }
}

#[derive(Clone)]
struct DaemonBridge {
    socket_path: PathBuf,
    timeout: Duration,
}

#[derive(Debug, Error)]
enum BridgeError {
    #[error("agent connection failed")]
    Connection(#[from] keith_connection::ConnectionError),
    #[error("agent protocol handshake failed")]
    Handshake,
    #[error("agent response was invalid")]
    Response,
    #[error("command scope was denied")]
    Scope,
    #[error("agent response serialization failed")]
    Serialize(#[from] serde_json::Error),
}

struct NativeClient {
    transport: FramedTransport<LocalStream>,
    client_id: ClientId,
    protocol: keith_agent_types::ProtocolVersion,
    server_hello: keith_protocol::ServerHello,
}

impl DaemonBridge {
    fn capabilities(&self) -> Result<BTreeSet<Feature>, BridgeError> {
        Ok(self.connect()?.server_hello.supported_features)
    }

    fn catalog(&self) -> Result<(Vec<ProfileSummary>, Vec<SessionSummary>), BridgeError> {
        let mut client = self.connect()?;
        let sessions = client.sessions(None)?;
        let mut profiles = sessions
            .iter()
            .map(|session| session.profile_id.clone())
            .collect::<BTreeSet<_>>()
            .into_iter()
            .map(|id| ProfileSummary {
                display_name: id.to_string(),
                id,
                workspace_id: keith_agent_types::WorkspaceId::new(),
                enabled: true,
            })
            .collect::<Vec<_>>();
        if let Ok(authoritative) = client.profiles() {
            profiles = authoritative;
        }
        Ok((profiles, sessions))
    }

    fn profile_exists(&self, profile: &ProfileId) -> Result<bool, BridgeError> {
        Ok(self
            .connect()?
            .profiles()?
            .iter()
            .any(|candidate| &candidate.id == profile && candidate.enabled))
    }

    fn session_in_profile(
        &self,
        profile: &ProfileId,
        session: &SessionId,
    ) -> Result<bool, BridgeError> {
        Ok(self
            .connect()?
            .sessions(Some(profile.clone()))?
            .iter()
            .any(|candidate| &candidate.session_id == session))
    }

    fn execute_scoped(
        &self,
        profile: &ProfileId,
        envelope: CommandEnvelope,
    ) -> Result<CommandResultEnvelope, BridgeError> {
        let mut client = self.connect()?;
        validate_command_scope(&mut client, profile, &envelope)?;
        client.execute(envelope)
    }

    fn execute_evolution(
        &self,
        command: EvolutionCommand,
    ) -> Result<CommandResultEnvelope, BridgeError> {
        let mut client = self.connect()?;
        let envelope = client.envelope(None, ClientCommand::Evolution(command));
        client.execute(envelope)
    }

    fn execute_scoped_streaming(
        &self,
        profile: &ProfileId,
        envelope: CommandEnvelope,
        events: &mut dyn FnMut(WireMessage),
    ) -> Result<CommandResultEnvelope, BridgeError> {
        let mut client = self.connect()?;
        validate_command_scope(&mut client, profile, &envelope)?;
        client.execute_streaming(envelope, events)
    }

    fn subscribe(
        &self,
        profile: &ProfileId,
        session: &SessionId,
        resume: ResumeQuery,
        output: &mpsc::Sender<String>,
    ) -> Result<(), BridgeError> {
        let mut client = self.connect()?;
        if !client
            .sessions(Some(profile.clone()))?
            .iter()
            .any(|candidate| &candidate.session_id == session)
        {
            return Err(BridgeError::Scope);
        }
        send_bounded(
            output,
            &WireMessage::ServerHello(client.server_hello.clone()),
        )?;
        let root_tree_id = client
            .sessions(Some(profile.clone()))?
            .into_iter()
            .find(|candidate| &candidate.session_id == session)
            .ok_or(BridgeError::Scope)?
            .root_tree_id;
        let cursor = match (resume.generation, resume.sequence) {
            (Some(generation), Some(sequence)) => Some(ResumeCursor {
                root_tree_id,
                generation: keith_agent_types::Generation::new(generation),
                last_sequence: Sequence::new(sequence),
            }),
            _ => None,
        };
        let envelope = client.envelope(
            Some(session.clone()),
            ClientCommand::AttachSession(AttachSession {
                session_id: session.clone(),
                resume: cursor,
            }),
        );
        let command_id = envelope.command_id.clone();
        client.transport.send(&WireMessage::Command(envelope))?;
        let mut buffered_events = Vec::new();
        loop {
            match client.transport.receive() {
                Ok(
                    message @ (WireMessage::Event(_)
                    | WireMessage::Snapshot(_)
                    | WireMessage::Terminal(_)),
                ) => buffered_events.push(message),
                Ok(WireMessage::CommandResult(result)) if result.command_id == command_id => {
                    let message = WireMessage::CommandResult(result);
                    send_bounded(output, &message)?;
                    for event in buffered_events {
                        send_bounded(output, &event)?;
                    }
                    break;
                }
                Ok(WireMessage::CommandResult(_)) => return Err(BridgeError::Response),
                Ok(_) => {}
                Err(error) if connection_timed_out(&error) => {
                    if output.is_closed() {
                        return Ok(());
                    }
                }
                Err(error) => return Err(error.into()),
            }
        }
        while !output.is_closed() {
            match client.transport.receive() {
                Ok(
                    message @ (WireMessage::Event(_)
                    | WireMessage::Snapshot(_)
                    | WireMessage::Terminal(_)),
                ) => send_bounded(output, &message)?,
                Ok(_) => {}
                Err(keith_connection::ConnectionError::Closed) => return Ok(()),
                Err(error) if connection_timed_out(&error) => {}
                Err(error) => return Err(error.into()),
            }
        }
        Ok(())
    }

    fn connect(&self) -> Result<NativeClient, BridgeError> {
        let stream = connect_local(&self.socket_path)?;
        set_local_read_timeout(&stream, Some(self.timeout))
            .map_err(keith_connection::ConnectionError::from)?;
        set_local_write_timeout(&stream, Some(self.timeout))
            .map_err(keith_connection::ConnectionError::from)?;
        let mut transport = FramedTransport::new(stream, WireFormat::Json);
        let client_id = ClientId::new();
        transport.send(&WireMessage::ClientHello(ClientHello {
            protocol: CURRENT_PROTOCOL_VERSION,
            client_id: client_id.clone(),
            client_name: "agent-web".into(),
            client_version: env!("CARGO_PKG_VERSION").into(),
            supported_features: BTreeSet::from([
                Feature::SessionLifecycle,
                Feature::Branching,
                Feature::Steering,
                Feature::Goals,
                Feature::Children,
                Feature::Schedules,
                Feature::MemoryQueries,
                Feature::Confirmations,
                Feature::Export,
                Feature::BackgroundControls,
                Feature::Replay,
                Feature::Snapshots,
                Feature::FramedJson,
                Feature::SelfEvolution,
            ]),
            resume: None,
        }))?;
        let WireMessage::ServerHello(server_hello) = transport.receive()? else {
            return Err(BridgeError::Handshake);
        };
        Ok(NativeClient {
            transport,
            client_id,
            protocol: server_hello.protocol,
            server_hello,
        })
    }
}

impl NativeClient {
    fn envelope(&self, session_id: Option<SessionId>, command: ClientCommand) -> CommandEnvelope {
        CommandEnvelope {
            protocol: self.protocol,
            command_id: CommandId::new(),
            client_id: self.client_id.clone(),
            sent_at: UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH),
            session_id,
            command,
        }
    }

    fn execute(&mut self, envelope: CommandEnvelope) -> Result<CommandResultEnvelope, BridgeError> {
        self.execute_streaming(envelope, &mut |_| {})
    }

    fn execute_streaming(
        &mut self,
        mut envelope: CommandEnvelope,
        events: &mut dyn FnMut(WireMessage),
    ) -> Result<CommandResultEnvelope, BridgeError> {
        envelope.protocol = self.protocol;
        envelope.client_id.clone_from(&self.client_id);
        let command_id = envelope.command_id.clone();
        self.transport.send(&WireMessage::Command(envelope))?;
        loop {
            match self.transport.receive()? {
                WireMessage::CommandResult(result) if result.command_id == command_id => {
                    return Ok(result);
                }
                message @ (WireMessage::Event(_)
                | WireMessage::Snapshot(_)
                | WireMessage::Terminal(_)) => events(message),
                _ => return Err(BridgeError::Response),
            }
        }
    }

    fn profiles(&mut self) -> Result<Vec<ProfileSummary>, BridgeError> {
        let envelope = self.envelope(None, ClientCommand::ListProfiles);
        let result = self.execute(envelope)?;
        match result.result {
            CommandResult::Data(payload) => match *payload {
                ResponsePayload::Profiles(profiles) => Ok(profiles),
                _ => Err(BridgeError::Response),
            },
            _ => Err(BridgeError::Response),
        }
    }

    fn sessions(
        &mut self,
        profile_id: Option<ProfileId>,
    ) -> Result<Vec<SessionSummary>, BridgeError> {
        let envelope = self.envelope(
            None,
            ClientCommand::ListSessions(SessionFilter {
                profile_id,
                include_archived: true,
            }),
        );
        let result = self.execute(envelope)?;
        match result.result {
            CommandResult::Data(payload) => match *payload {
                ResponsePayload::Sessions(sessions) => Ok(sessions),
                _ => Err(BridgeError::Response),
            },
            _ => Err(BridgeError::Response),
        }
    }
}

fn validate_command_scope(
    client: &mut NativeClient,
    profile: &ProfileId,
    envelope: &CommandEnvelope,
) -> Result<(), BridgeError> {
    if matches!(envelope.command, ClientCommand::ListProfiles) {
        return Err(BridgeError::Scope);
    }
    if let Some(command_profile) = command_profile(&envelope.command)
        && command_profile != profile
    {
        return Err(BridgeError::Scope);
    }
    if !command_scope_is_internally_consistent(&envelope.command) {
        return Err(BridgeError::Scope);
    }
    let embedded_session = command_session(&envelope.command);
    if let (Some(outer), Some(inner)) = (&envelope.session_id, embedded_session)
        && outer != inner
    {
        return Err(BridgeError::Scope);
    }
    let session = embedded_session.or(envelope.session_id.as_ref());
    if command_requires_session(&envelope.command) && session.is_none() {
        return Err(BridgeError::Scope);
    }
    if let Some(session) = session
        && !client
            .sessions(Some(profile.clone()))?
            .iter()
            .any(|candidate| &candidate.session_id == session)
    {
        return Err(BridgeError::Scope);
    }
    Ok(())
}

fn command_profile(command: &ClientCommand) -> Option<&ProfileId> {
    match command {
        ClientCommand::ListSessions(filter) => filter.profile_id.as_ref(),
        ClientCommand::CreateSession(request) => Some(&request.profile_id),
        ClientCommand::CreateSchedule(request) => Some(&request.profile_id),
        ClientCommand::QueryMemory(request) => Some(&request.profile_id),
        ClientCommand::SetBackgroundControl(request) => Some(&request.profile_id),
        ClientCommand::ChannelAccount(command) => Some(match command {
            keith_protocol::ChannelAccountCommand::List { profile_id }
            | keith_protocol::ChannelAccountCommand::Inspect { profile_id, .. }
            | keith_protocol::ChannelAccountCommand::Test { profile_id, .. }
            | keith_protocol::ChannelAccountCommand::Pause { profile_id, .. }
            | keith_protocol::ChannelAccountCommand::Resume { profile_id, .. }
            | keith_protocol::ChannelAccountCommand::RotateCredentials { profile_id, .. }
            | keith_protocol::ChannelAccountCommand::Remove { profile_id, .. } => profile_id,
            keith_protocol::ChannelAccountCommand::Connect(configuration)
            | keith_protocol::ChannelAccountCommand::Configure(configuration) => {
                &configuration.profile_id
            }
        }),
        ClientCommand::Integration(command) => Some(match command {
            keith_protocol::IntegrationCommand::List { profile_id, .. }
            | keith_protocol::IntegrationCommand::Inspect { profile_id, .. } => profile_id,
            keith_protocol::IntegrationCommand::Mutate(mutation) => &mutation.profile_id,
        }),
        ClientCommand::HarnessRepair(command) => Some(match command {
            keith_protocol::HarnessRepairCommand::Refresh { profile_id }
            | keith_protocol::HarnessRepairCommand::SetMode { profile_id, .. }
            | keith_protocol::HarnessRepairCommand::Approve { profile_id, .. }
            | keith_protocol::HarnessRepairCommand::Promote { profile_id, .. }
            | keith_protocol::HarnessRepairCommand::Reverse { profile_id, .. }
            | keith_protocol::HarnessRepairCommand::RetryCurrentTask { profile_id, .. } => {
                profile_id
            }
        }),
        _ => None,
    }
}

fn command_scope_is_internally_consistent(command: &ClientCommand) -> bool {
    match command {
        ClientCommand::Integration(keith_protocol::IntegrationCommand::Mutate(mutation)) => {
            mutation.profile_id == mutation.authority.profile_id
        }
        _ => true,
    }
}

fn command_session(command: &ClientCommand) -> Option<&SessionId> {
    match command {
        ClientCommand::AttachSession(request) => Some(&request.session_id),
        ClientCommand::DetachSession { session_id }
        | ClientCommand::ResumeSession { session_id }
        | ClientCommand::ListGoals { session_id }
        | ClientCommand::ListChildren { session_id } => Some(session_id),
        ClientCommand::BranchSession(request) => Some(&request.session_id),
        ClientCommand::SelectBranch(request) => Some(&request.session_id),
        ClientCommand::SubmitPrompt(request) => Some(&request.session_id),
        ClientCommand::Steer(request) => Some(&request.session_id),
        ClientCommand::SelectModel(request) => Some(&request.session_id),
        ClientCommand::CreateGoal(request) => Some(&request.session_id),
        ClientCommand::CreateChild(request) => Some(&request.parent_session_id),
        ClientCommand::CreateSchedule(request) => request.session_id.as_ref(),
        ClientCommand::Export(request) => Some(&request.session_id),
        ClientCommand::StageAttachment(request) => Some(&request.session_id),
        ClientCommand::Integration(keith_protocol::IntegrationCommand::Mutate(mutation)) => {
            Some(&mutation.authority.session_id)
        }
        ClientCommand::Cancel(keith_protocol::CancelTarget::Session(session_id)) => {
            Some(session_id)
        }
        _ => None,
    }
}

fn command_requires_session(command: &ClientCommand) -> bool {
    !matches!(
        command,
        ClientCommand::ListProfiles
            | ClientCommand::ListSessions(_)
            | ClientCommand::CreateSession(_)
            | ClientCommand::CreateSchedule(_)
            | ClientCommand::QueryMemory(_)
            | ClientCommand::SetBackgroundControl(_)
    )
}

fn send_bounded(output: &mpsc::Sender<String>, message: &WireMessage) -> Result<(), BridgeError> {
    let encoded = serde_json::to_string(message)?;
    output.try_send(encoded).map_err(|_| BridgeError::Response)
}

fn connection_timed_out(error: &keith_connection::ConnectionError) -> bool {
    match error {
        keith_connection::ConnectionError::Io(error)
        | keith_connection::ConnectionError::Frame(FrameError::Io(error)) => matches!(
            error.kind(),
            std::io::ErrorKind::WouldBlock | std::io::ErrorKind::TimedOut
        ),
        _ => false,
    }
}

fn validate_config(config: &WebServerConfig) -> Result<(), ServerError> {
    let origin = Url::parse(&config.exact_origin)
        .map_err(|_| ServerError::Configuration("exact origin is invalid".into()))?;
    if !matches!(origin.scheme(), "http" | "https")
        || origin.host_str().is_none()
        || origin.username() != ""
        || origin.password().is_some()
        || origin.query().is_some()
        || origin.fragment().is_some()
        || origin.path() != "/"
        || config.login_secret.is_empty()
        || config.session_lifetime.is_zero()
        || config.daemon_timeout.is_zero()
        || config
            .openai_compatibility
            .as_ref()
            .is_some_and(|compatibility| {
                compatibility.api_key.len() < 32
                    || compatibility.max_in_flight == 0
                    || (!config.bind.ip().is_loopback() && !compatibility.allow_non_loopback)
            })
        || config
            .platform_compatibility
            .as_ref()
            .is_some_and(|compatibility| {
                compatibility.api_key.len() < 32
                    || compatibility.max_in_flight == 0
                    || (!config.bind.ip().is_loopback() && !compatibility.allow_non_loopback)
            })
    {
        return Err(ServerError::Configuration(
            "origin, secrets, or limits are invalid".into(),
        ));
    }
    Ok(())
}

fn parse_boolean(value: &str) -> Result<bool, String> {
    match value {
        "true" => Ok(true),
        "false" => Ok(false),
        _ => Err("boolean values must be true or false".to_owned()),
    }
}

fn decode_key(encoded: &[u8]) -> Result<[u8; 32], String> {
    if encoded.len() != 64 {
        return Err("credential key must be 64 hexadecimal characters".into());
    }
    let mut key = [0_u8; 32];
    for (index, pair) in encoded.chunks_exact(2).enumerate() {
        let high = hex_digit(pair[0])?;
        let low = hex_digit(pair[1])?;
        key[index] = (high << 4) | low;
    }
    Ok(key)
}

fn hex_digit(value: u8) -> Result<u8, String> {
    match value {
        b'0'..=b'9' => Ok(value - b'0'),
        b'a'..=b'f' => Ok(value - b'a' + 10),
        b'A'..=b'F' => Ok(value - b'A' + 10),
        _ => Err("credential key must be hexadecimal".into()),
    }
}

fn next_app_page(root: &FsPath) -> Response {
    let path = root.join(UI_INDEX);
    let Ok(mut html) = std::fs::read_to_string(path) else {
        return safe_error(
            StatusCode::SERVICE_UNAVAILABLE,
            "Keith's Next.js interface is unavailable",
        );
    };
    let nonce = keith_agent_types::EntityId::new().to_string();
    html = html.replace("<script", &format!("<script nonce=\"{nonce}\""));
    let mut response = html.into_response();
    response.headers_mut().insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("text/html; charset=utf-8"),
    );
    let policy = format!(
        "default-src 'self'; script-src 'self' 'nonce-{nonce}'; connect-src 'self' ws: wss:; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' https://cdn.openai.com; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"
    );
    response.headers_mut().insert(
        header::CONTENT_SECURITY_POLICY,
        HeaderValue::from_str(&policy)
            .unwrap_or_else(|_| HeaderValue::from_static("default-src 'none'")),
    );
    response
        .headers_mut()
        .insert(header::CACHE_CONTROL, HeaderValue::from_static("no-store"));
    response.headers_mut().insert(
        header::X_CONTENT_TYPE_OPTIONS,
        HeaderValue::from_static("nosniff"),
    );
    response
}

fn safe_ui_asset_path(path: &str) -> Option<PathBuf> {
    let path = FsPath::new(path);
    if path.as_os_str().is_empty()
        || path
            .components()
            .any(|component| !matches!(component, std::path::Component::Normal(_)))
    {
        return None;
    }
    Some(path.to_path_buf())
}

fn file_asset(root: &FsPath, filename: &FsPath, media_type: &'static str) -> Response {
    match std::fs::read(root.join(filename)) {
        Ok(bytes) => asset_response(media_type, bytes, false),
        Err(_) => safe_error(StatusCode::NOT_FOUND, "asset unavailable"),
    }
}

fn asset_response(media_type: &'static str, bytes: Vec<u8>, no_store: bool) -> Response {
    let mut response = bytes.into_response();
    response
        .headers_mut()
        .insert(header::CONTENT_TYPE, HeaderValue::from_static(media_type));
    response.headers_mut().insert(
        header::CACHE_CONTROL,
        HeaderValue::from_static(if no_store {
            "no-store"
        } else {
            "public, max-age=31536000, immutable"
        }),
    );
    response.headers_mut().insert(
        header::X_CONTENT_TYPE_OPTIONS,
        HeaderValue::from_static("nosniff"),
    );
    response
}

fn security_response(error: SecurityError) -> Response {
    let status = match error {
        SecurityError::Authentication => StatusCode::UNAUTHORIZED,
        SecurityError::Origin | SecurityError::Csrf => StatusCode::FORBIDDEN,
        SecurityError::RateLimit => StatusCode::TOO_MANY_REQUESTS,
        SecurityError::Random | SecurityError::Lock => StatusCode::SERVICE_UNAVAILABLE,
    };
    safe_error(status, &error.to_string())
}

fn safe_error(status: StatusCode, message: &str) -> Response {
    (status, message.to_owned()).into_response()
}

async fn shutdown_signal() {
    let _ = tokio::signal::ctrl_c().await;
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn arguments_require_secret_environment_names_not_secret_values() {
        let parsed = ServerArguments::parse([
            "agent-web",
            "--socket",
            "/tmp/agent.sock",
            "--credential-root",
            "/tmp/credentials",
            "--login-secret-env",
            "TEST_LOGIN",
            "--credential-key-env",
            "TEST_KEY",
            "--openai-api-key-env",
            "TEST_OPENAI_COMPAT",
            "--openai-allow-non-loopback",
            "true",
        ])
        .unwrap()
        .unwrap();
        assert_eq!(parsed.login_secret_env, "TEST_LOGIN");
        assert_eq!(parsed.openai_api_key_env, "TEST_OPENAI_COMPAT");
        assert!(parsed.openai_allow_non_loopback);
        assert_eq!(
            parsed.credential_key_source,
            CredentialKeySource::Environment("TEST_KEY".into())
        );
        assert!(!format!("{parsed:?}").contains("secret-value"));
        let native = ServerArguments::parse([
            "agent-web",
            "--socket",
            "/tmp/agent.sock",
            "--credential-root",
            "/tmp/credentials",
            "--credential-key-native-account",
            "desktop",
        ])
        .unwrap()
        .unwrap();
        assert_eq!(
            native.credential_key_source,
            CredentialKeySource::Native {
                service: "keith-agent".into(),
                account: "desktop".into(),
            }
        );
        let defaults = ServerArguments::parse(["agent-web"]).unwrap().unwrap();
        assert!(defaults.daemon_socket.is_absolute());
        assert!(defaults.credential_root.is_absolute());
        assert!(matches!(
            defaults.credential_key_source,
            CredentialKeySource::Restricted(_)
        ));
    }

    #[test]
    fn command_scope_requires_matching_profile_and_session() {
        let profile = ProfileId::new();
        let other = ProfileId::new();
        let session = SessionId::new();
        let command = ClientCommand::SubmitPrompt(keith_protocol::SubmitPrompt {
            session_id: session.clone(),
            text: "hello".into(),
            artifacts: Vec::new(),
            delivery: keith_protocol::DeliveryPolicy::Immediate,
            reply_route: None,
        });
        assert_eq!(command_session(&command), Some(&session));
        assert!(command_profile(&command).is_none());
        let query = ClientCommand::QueryMemory(keith_protocol::MemoryQuery {
            profile_id: other.clone(),
            query: "term".into(),
            limit: 10,
        });
        assert_eq!(command_profile(&query), Some(&other));
        assert_ne!(command_profile(&query), Some(&profile));

        let integration = ClientCommand::Integration(keith_protocol::IntegrationCommand::List {
            profile_id: other.clone(),
            service: Some(keith_protocol::IntegrationService::ConnectedApp),
        });
        assert_eq!(command_profile(&integration), Some(&other));
        assert_ne!(command_profile(&integration), Some(&profile));
        assert!(command_scope_is_internally_consistent(&integration));

        let harness = ClientCommand::HarnessRepair(keith_protocol::HarnessRepairCommand::Refresh {
            profile_id: other.clone(),
        });
        assert_eq!(command_profile(&harness), Some(&other));

        let channel = ClientCommand::ChannelAccount(keith_protocol::ChannelAccountCommand::List {
            profile_id: other.clone(),
        });
        assert_eq!(command_profile(&channel), Some(&other));
    }

    #[test]
    fn requested_session_is_prioritized_only_when_it_exists() {
        let profile_id = ProfileId::new();
        let first = SessionSummary {
            session_id: SessionId::new(),
            root_tree_id: keith_agent_types::RootTreeId::new(),
            profile_id: profile_id.clone(),
            title: Some("first".into()),
            state: keith_protocol::SessionState::Ready,
            updated_at: UtcTimestamp::UNIX_EPOCH,
        };
        let selected = SessionSummary {
            session_id: SessionId::new(),
            root_tree_id: keith_agent_types::RootTreeId::new(),
            profile_id,
            title: Some("selected".into()),
            state: keith_protocol::SessionState::Ready,
            updated_at: UtcTimestamp::UNIX_EPOCH,
        };
        let mut sessions = vec![first.clone(), selected.clone()];
        prioritize_session(&mut sessions, Some(&selected.session_id.to_string()));
        assert_eq!(sessions[0].session_id, selected.session_id);
        prioritize_session(&mut sessions, Some("not-a-session"));
        assert_eq!(sessions[0].session_id, selected.session_id);
    }

    #[test]
    fn configuration_debug_and_key_errors_do_not_expose_secrets() {
        let config = WebServerConfig {
            bind: "127.0.0.1:7341".parse().unwrap(),
            exact_origin: "http://127.0.0.1:7341".into(),
            daemon_socket: "/tmp/daemon.sock".into(),
            asset_root: "/tmp/assets".into(),
            credential_root: "/tmp/credentials".into(),
            credential_key: MasterKey::from_bytes([3; 32]),
            login_secret: b"diagnostic-secret".to_vec(),
            session_lifetime: Duration::from_secs(30),
            mutation_limit_per_second: 4,
            daemon_timeout: Duration::from_secs(1),
            openai_compatibility: Some(OpenAiCompatibilityConfig {
                api_key: b"openai-compatibility-diagnostic-secret".to_vec(),
                allow_non_loopback: false,
                max_in_flight: 2,
            }),
            platform_compatibility: Some(PlatformCompatibilityConfig {
                api_key: b"platform-compatibility-diagnostic-secret".to_vec(),
                allow_non_loopback: false,
                max_in_flight: 2,
            }),
        };
        assert!(!format!("{config:?}").contains("diagnostic-secret"));
        assert!(decode_key(b"not-a-key").unwrap_err().contains("64"));
        let mut exposed = config;
        exposed.bind = "0.0.0.0:7341".parse().unwrap();
        assert!(validate_config(&exposed).is_err());
        exposed
            .openai_compatibility
            .as_mut()
            .unwrap()
            .allow_non_loopback = true;
        exposed
            .platform_compatibility
            .as_mut()
            .unwrap()
            .allow_non_loopback = true;
        assert!(validate_config(&exposed).is_ok());
    }

    #[test]
    fn next_production_assets_are_loaded_without_path_escape() {
        let root = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(root.path().join("ui")).unwrap();
        std::fs::write(
            root.path().join(UI_INDEX),
            r#"<!doctype html><html><body><script src="/assets/ui/app.js"></script></body></html>"#,
        )
        .unwrap();
        let response = next_app_page(root.path());
        assert_eq!(response.status(), StatusCode::OK);
        let policy = response
            .headers()
            .get(header::CONTENT_SECURITY_POLICY)
            .unwrap()
            .to_str()
            .unwrap();
        assert!(policy.contains("nonce-"));
        assert!(!policy.contains("wasm-unsafe-eval"));
        assert!(safe_ui_asset_path("_next/static/keith-a.js").is_some());
        assert!(safe_ui_asset_path("../agent_web.js").is_none());
        assert!(safe_ui_asset_path("/etc/passwd").is_none());
    }

    #[test]
    fn browser_command_streaming_requires_an_explicit_sse_accept_type() {
        let mut headers = HeaderMap::new();
        assert!(!accepts_event_stream(&headers));
        headers.insert(
            header::ACCEPT,
            HeaderValue::from_static("application/json, text/event-stream"),
        );
        assert!(accepts_event_stream(&headers));
        headers.insert(header::ACCEPT, HeaderValue::from_static("application/json"));
        assert!(!accepts_event_stream(&headers));
    }

    #[test]
    fn installation_evolution_body_cannot_supply_identity_authority_or_profile_scope() {
        assert_eq!(
            parse_evolution_command(br#"{"action":"status"}"#).unwrap(),
            EvolutionCommand::Status
        );
        for injected in [
            br#"{"action":"status","identity":"owner"}"#.as_slice(),
            br#"{"action":"status","authority":"installation"}"#.as_slice(),
            br#"{"action":"status","profile_id":"01J00000000000000000000000"}"#.as_slice(),
            br#"{"action":"revert","parameters":{"promotion_id":"01J00000000000000000000000","reason":"undo","identity":"owner"}}"#.as_slice(),
        ] {
            assert!(parse_evolution_command(injected).is_err());
        }
    }
}
