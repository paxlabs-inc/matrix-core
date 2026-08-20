#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Write as _;
use std::fs;
use std::io::{BufRead, BufReader, Read, Write};
use std::net::TcpStream;
#[cfg(unix)]
use std::os::unix::process::CommandExt;
#[cfg(windows)]
use std::os::windows::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::{Arc, mpsc};
use std::thread;
use std::time::Duration;

use keith_agent_types::{ProfileId, SessionId, UtcTimestamp};
use keith_credentials::{
    CredentialError, CredentialOwner, CredentialRef, EncryptedCredentialStore,
};
#[cfg(unix)]
use nix::sys::signal::{Signal, killpg};
#[cfg(unix)]
use nix::unistd::Pid;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use thiserror::Error;
use url::Url;

const STATE_FILE: &str = "mcp-state.json";

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "placement", content = "name")]
pub enum McpAuthentication {
    Header(String),
    Environment(String),
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct McpCredential {
    pub reference: CredentialRef,
    pub placement: McpAuthentication,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "transport")]
pub enum McpTransport {
    Stdio {
        executable: PathBuf,
        args: Vec<String>,
        working_directory: Option<PathBuf>,
        environment: BTreeMap<String, String>,
    },
    Http {
        endpoint: String,
        headers: BTreeMap<String, String>,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct McpServerConfig {
    pub id: String,
    pub transport: McpTransport,
    pub enabled_profiles: BTreeSet<ProfileId>,
    pub credential: Option<McpCredential>,
    pub allowed_filesystem_roots: Vec<PathBuf>,
    pub allowed_network_hosts: BTreeSet<String>,
    pub timeout_ms: u64,
    pub max_request_bytes: usize,
    pub max_response_bytes: usize,
    pub max_tools: usize,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct McpToolSchema {
    pub server_id: String,
    pub name: String,
    pub description: String,
    pub input_schema: Value,
    #[serde(default)]
    pub embedding: Vec<i32>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SchemaCache {
    pub server_id: String,
    pub version: u64,
    pub digest: String,
    pub updated_at: UtcTimestamp,
    pub tools: Vec<McpToolSchema>,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct McpToolResult {
    pub content: Vec<Value>,
    pub is_error: bool,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ServerHealth {
    Unknown,
    Healthy,
    Unhealthy,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ServerStatus {
    pub health: ServerHealth,
    pub generation: u64,
    pub safe_error: Option<String>,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct DurableState {
    configs: BTreeMap<String, McpServerConfig>,
    caches: BTreeMap<String, SchemaCache>,
    statuses: BTreeMap<String, ServerStatus>,
}

#[derive(Clone, Debug)]
struct McpSession {
    profile_id: ProfileId,
    server_id: String,
}

#[derive(Debug, Error)]
pub enum McpError {
    #[error("MCP configuration is invalid or authority was not explicitly granted")]
    InvalidConfiguration,
    #[error("MCP server or session was not found")]
    NotFound,
    #[error("MCP server is not enabled for this profile")]
    ProfileDenied,
    #[error("MCP credential authentication failed")]
    Authentication,
    #[error("MCP session capacity was reached")]
    SessionLimit,
    #[error("MCP request timed out")]
    Timeout,
    #[error("MCP request or response exceeded its configured bound")]
    SizeLimit,
    #[error("MCP transport failed: {0}")]
    Transport(String),
    #[error("MCP protocol response is malformed")]
    Protocol,
    #[error("MCP server returned an error")]
    Remote,
    #[error("MCP persistence failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("MCP state JSON failed: {0}")]
    Json(#[from] serde_json::Error),
}

impl From<CredentialError> for McpError {
    fn from(_: CredentialError) -> Self {
        Self::Authentication
    }
}

pub struct McpManager {
    root: PathBuf,
    credentials: Arc<EncryptedCredentialStore>,
    state: DurableState,
    sessions: BTreeMap<(SessionId, String), McpSession>,
    max_sessions: usize,
}

impl McpManager {
    /// Opens daemon-owned MCP configuration, health, and schema state.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid capacity or corrupt durable state.
    pub fn open(
        root: impl AsRef<Path>,
        credentials: Arc<EncryptedCredentialStore>,
        max_sessions: usize,
    ) -> Result<Self, McpError> {
        if max_sessions == 0 {
            return Err(McpError::SessionLimit);
        }
        fs::create_dir_all(root.as_ref())?;
        let root = fs::canonicalize(root.as_ref())?;
        let state_path = root.join(STATE_FILE);
        let state = if state_path.exists() {
            serde_json::from_slice(&fs::read(state_path)?)?
        } else {
            DurableState::default()
        };
        Ok(Self {
            root,
            credentials,
            state,
            sessions: BTreeMap::new(),
            max_sessions,
        })
    }

    /// Adds or replaces a validated external trust-domain configuration.
    ///
    /// # Errors
    ///
    /// Returns an error when authority or bounds are invalid.
    pub fn configure(&mut self, config: McpServerConfig) -> Result<(), McpError> {
        validate_config(&config)?;
        let id = config.id.clone();
        self.state.configs.insert(id.clone(), config);
        self.state.statuses.entry(id).or_insert(ServerStatus {
            health: ServerHealth::Unknown,
            generation: 0,
            safe_error: None,
        });
        self.persist()
    }

    pub fn status(&self, id: &str) -> Option<&ServerStatus> {
        self.state.statuses.get(id)
    }

    pub fn cache(&self, id: &str) -> Option<&SchemaCache> {
        self.state.caches.get(id)
    }

    /// Opens a bounded normalized session after profile authorization.
    ///
    /// # Errors
    ///
    /// Returns an error for unknown, disabled, or saturated servers.
    pub fn open_session(
        &mut self,
        session_id: &SessionId,
        profile_id: ProfileId,
        server_id: &str,
    ) -> Result<(), McpError> {
        let config = self
            .state
            .configs
            .get(server_id)
            .ok_or(McpError::NotFound)?;
        if !config.enabled_profiles.contains(&profile_id) {
            return Err(McpError::ProfileDenied);
        }
        let key = (session_id.clone(), server_id.to_owned());
        if !self.sessions.contains_key(&key) && self.sessions.len() >= self.max_sessions {
            return Err(McpError::SessionLimit);
        }
        self.sessions.insert(
            key,
            McpSession {
                profile_id,
                server_id: server_id.to_owned(),
            },
        );
        Ok(())
    }

    pub fn close_session(&mut self, session_id: &SessionId) -> bool {
        let before = self.sessions.len();
        self.sessions
            .retain(|(candidate, _), _| candidate != session_id);
        self.sessions.len() != before
    }

    pub fn cleanup_sessions(&mut self) -> usize {
        let count = self.sessions.len();
        self.sessions.clear();
        count
    }

    /// Refreshes and versions the bounded tool schema cache.
    ///
    /// # Errors
    ///
    /// Returns an error for authentication, transport, protocol, or bounds failure.
    pub fn refresh_schema(
        &mut self,
        server_id: &str,
        now: UtcTimestamp,
    ) -> Result<&SchemaCache, McpError> {
        let config = self
            .state
            .configs
            .get(server_id)
            .cloned()
            .ok_or(McpError::NotFound)?;
        let response = self.request(&config, "tools/list", &json!({}))?;
        let tools_value = response
            .get("result")
            .and_then(|result| result.get("tools"))
            .cloned()
            .ok_or(McpError::Protocol)?;
        let raw_tools = tools_value.as_array().ok_or(McpError::Protocol)?;
        if raw_tools.len() > config.max_tools {
            return Err(McpError::SizeLimit);
        }
        let mut tools = Vec::with_capacity(raw_tools.len());
        for raw in raw_tools {
            tools.push(McpToolSchema {
                server_id: server_id.to_owned(),
                name: required_string(raw, "name")?,
                description: required_string(raw, "description")?,
                input_schema: raw.get("inputSchema").cloned().ok_or(McpError::Protocol)?,
                embedding: raw
                    .get("embedding")
                    .and_then(Value::as_array)
                    .map(|values| {
                        values
                            .iter()
                            .map(|value| {
                                value
                                    .as_i64()
                                    .and_then(|number| i32::try_from(number).ok())
                                    .ok_or(McpError::Protocol)
                            })
                            .collect::<Result<Vec<_>, _>>()
                    })
                    .transpose()?
                    .unwrap_or_default(),
            });
        }
        tools.sort_by(|left, right| left.name.cmp(&right.name));
        let digest = digest_json(&tools)?;
        let version = self
            .state
            .caches
            .get(server_id)
            .map_or(1, |cache| cache.version + u64::from(cache.digest != digest));
        self.state.caches.insert(
            server_id.to_owned(),
            SchemaCache {
                server_id: server_id.to_owned(),
                version,
                digest,
                updated_at: now,
                tools,
            },
        );
        self.mark_health(server_id, ServerHealth::Healthy, None)?;
        self.state.caches.get(server_id).ok_or(McpError::Protocol)
    }

    /// Reconnects by performing a fresh bounded schema journey and increasing generation.
    ///
    /// # Errors
    ///
    /// Returns the underlying connection failure after recording unhealthy state.
    pub fn reconnect(&mut self, server_id: &str, now: UtcTimestamp) -> Result<(), McpError> {
        if let Some(status) = self.state.statuses.get_mut(server_id) {
            status.generation = status.generation.saturating_add(1);
        } else {
            return Err(McpError::NotFound);
        }
        match self.refresh_schema(server_id, now) {
            Ok(_) => Ok(()),
            Err(error) => {
                self.mark_health(server_id, ServerHealth::Unhealthy, Some(error.to_string()))?;
                Err(error)
            }
        }
    }

    /// Selects enabled lexical/vector-relevant schemas under an exact serialized-byte budget.
    pub fn relevant_tools(
        &self,
        profile_id: &ProfileId,
        query: &str,
        query_embedding: &[i32],
        max_bytes: usize,
    ) -> Vec<McpToolSchema> {
        let query_terms = terms(query);
        let mut scored = self
            .state
            .caches
            .values()
            .filter(|cache| {
                self.state
                    .configs
                    .get(&cache.server_id)
                    .is_some_and(|config| config.enabled_profiles.contains(profile_id))
            })
            .flat_map(|cache| cache.tools.iter())
            .map(|tool| {
                let text_terms = terms(&format!("{} {}", tool.name, tool.description));
                let lexical = i64::try_from(query_terms.intersection(&text_terms).count())
                    .unwrap_or(i64::MAX);
                let vector = dot_product(query_embedding, &tool.embedding);
                (lexical.saturating_mul(1_000).saturating_add(vector), tool)
            })
            .filter(|(score, _)| *score > 0)
            .collect::<Vec<_>>();
        scored.sort_by(|(left_score, left), (right_score, right)| {
            right_score
                .cmp(left_score)
                .then_with(|| left.server_id.cmp(&right.server_id))
                .then_with(|| left.name.cmp(&right.name))
        });
        let mut selected = Vec::new();
        let mut used = 0_usize;
        for (_, tool) in scored {
            let Ok(bytes) = serde_json::to_vec(tool) else {
                continue;
            };
            if used.saturating_add(bytes.len()) > max_bytes {
                continue;
            }
            used += bytes.len();
            selected.push(tool.clone());
        }
        selected
    }

    /// Executes one MCP call through an authorized normalized session.
    ///
    /// # Errors
    ///
    /// Returns an error for isolation, bounds, timeout, transport, or protocol failure.
    pub fn call_tool(
        &self,
        session_id: &SessionId,
        server_id: &str,
        tool_name: &str,
        arguments: &Value,
    ) -> Result<McpToolResult, McpError> {
        let session = self
            .sessions
            .get(&(session_id.clone(), server_id.to_owned()))
            .ok_or(McpError::NotFound)?;
        let config = self
            .state
            .configs
            .get(&session.server_id)
            .ok_or(McpError::NotFound)?;
        if !config.enabled_profiles.contains(&session.profile_id) {
            return Err(McpError::ProfileDenied);
        }
        let cache = self
            .state
            .caches
            .get(&session.server_id)
            .ok_or(McpError::NotFound)?;
        if !cache.tools.iter().any(|tool| tool.name == tool_name) {
            return Err(McpError::NotFound);
        }
        let response = self.request(
            config,
            "tools/call",
            &json!({"name": tool_name, "arguments": arguments}),
        )?;
        if response.get("error").is_some() {
            return Err(McpError::Remote);
        }
        let result = response.get("result").ok_or(McpError::Protocol)?;
        Ok(McpToolResult {
            content: result
                .get("content")
                .and_then(Value::as_array)
                .cloned()
                .ok_or(McpError::Protocol)?,
            is_error: result
                .get("isError")
                .and_then(Value::as_bool)
                .unwrap_or(false),
        })
    }

    fn request(
        &self,
        config: &McpServerConfig,
        method: &str,
        params: &Value,
    ) -> Result<Value, McpError> {
        let request = serde_json::to_vec(&json!({
            "jsonrpc": "2.0",
            "id": 1,
            "method": method,
            "params": params,
        }))?;
        if request.len() > config.max_request_bytes {
            return Err(McpError::SizeLimit);
        }
        let credential = config
            .credential
            .as_ref()
            .map(|credential| self.resolve_credential(&config.id, credential))
            .transpose()?;
        let response = match &config.transport {
            McpTransport::Stdio {
                executable,
                args,
                working_directory,
                environment,
            } => stdio_request(
                executable,
                args,
                working_directory.as_deref(),
                environment,
                config.credential.as_ref(),
                credential.as_deref(),
                &request,
                config.timeout_ms,
                config.max_response_bytes,
            )?,
            McpTransport::Http { endpoint, headers } => http_request(
                endpoint,
                headers,
                config.credential.as_ref(),
                credential.as_deref(),
                &request,
                config.timeout_ms,
                config.max_response_bytes,
            )?,
        };
        serde_json::from_slice(&response).map_err(|_| McpError::Protocol)
    }

    fn resolve_credential(
        &self,
        server_id: &str,
        credential: &McpCredential,
    ) -> Result<String, McpError> {
        let owner = CredentialOwner::Mcp(server_id.to_owned());
        let secret = self.credentials.resolve(&credential.reference, &owner)?;
        secret
            .with_bytes(|bytes| String::from_utf8(bytes.to_vec()))
            .map_err(|_| McpError::Authentication)
    }

    fn mark_health(
        &mut self,
        server_id: &str,
        health: ServerHealth,
        safe_error: Option<String>,
    ) -> Result<(), McpError> {
        let status = self
            .state
            .statuses
            .get_mut(server_id)
            .ok_or(McpError::NotFound)?;
        status.health = health;
        status.safe_error = safe_error;
        self.persist()
    }

    fn persist(&self) -> Result<(), McpError> {
        let temporary = self.root.join(format!(".{STATE_FILE}.tmp"));
        fs::write(&temporary, serde_json::to_vec_pretty(&self.state)?)?;
        keith_platform::replace_file(&temporary, &self.root.join(STATE_FILE))?;
        Ok(())
    }
}

fn validate_config(config: &McpServerConfig) -> Result<(), McpError> {
    let valid_id = !config.id.is_empty()
        && config
            .id
            .bytes()
            .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || byte == b'-');
    if !valid_id
        || config.enabled_profiles.is_empty()
        || config.timeout_ms == 0
        || config.max_request_bytes == 0
        || config.max_response_bytes == 0
        || config.max_tools == 0
    {
        return Err(McpError::InvalidConfiguration);
    }
    if let Some(credential) = &config.credential
        && credential.reference.owner != CredentialOwner::Mcp(config.id.clone())
    {
        return Err(McpError::InvalidConfiguration);
    }
    match &config.transport {
        McpTransport::Stdio {
            executable,
            working_directory,
            ..
        } => {
            if !executable.is_absolute() || !executable.is_file() {
                return Err(McpError::InvalidConfiguration);
            }
            if let Some(directory) = working_directory {
                let directory = fs::canonicalize(directory)?;
                let allowed = config
                    .allowed_filesystem_roots
                    .iter()
                    .filter_map(|root| fs::canonicalize(root).ok())
                    .any(|root| directory.starts_with(root));
                if !allowed {
                    return Err(McpError::InvalidConfiguration);
                }
            }
            if config.credential.as_ref().is_some_and(|credential| {
                !matches!(credential.placement, McpAuthentication::Environment(_))
            }) {
                return Err(McpError::InvalidConfiguration);
            }
        }
        McpTransport::Http { endpoint, headers } => {
            let endpoint = Url::parse(endpoint).map_err(|_| McpError::InvalidConfiguration)?;
            let host = endpoint.host_str().ok_or(McpError::InvalidConfiguration)?;
            if endpoint.scheme() != "http" || !config.allowed_network_hosts.contains(host) {
                return Err(McpError::InvalidConfiguration);
            }
            if headers.iter().any(|(name, value)| {
                name.is_empty()
                    || !name
                        .bytes()
                        .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
                    || value.contains(['\r', '\n'])
            }) {
                return Err(McpError::InvalidConfiguration);
            }
            if config.credential.as_ref().is_some_and(|credential| {
                !matches!(&credential.placement, McpAuthentication::Header(name)
                    if !name.is_empty()
                        && name.bytes().all(|byte| byte.is_ascii_alphanumeric() || byte == b'-'))
            }) {
                return Err(McpError::InvalidConfiguration);
            }
        }
    }
    Ok(())
}

#[allow(clippy::too_many_arguments)]
fn stdio_request(
    executable: &Path,
    args: &[String],
    working_directory: Option<&Path>,
    environment: &BTreeMap<String, String>,
    credential: Option<&McpCredential>,
    secret: Option<&str>,
    request: &[u8],
    timeout_ms: u64,
    max_response_bytes: usize,
) -> Result<Vec<u8>, McpError> {
    let mut command = Command::new(executable);
    command
        .args(args)
        .env_clear()
        .envs(environment)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::null());
    configure_process_group(&mut command);
    if let Some(directory) = working_directory {
        command.current_dir(directory);
    }
    if let (
        Some(McpCredential {
            placement: McpAuthentication::Environment(name),
            ..
        }),
        Some(value),
    ) = (credential, secret)
    {
        command.env(name, value);
    }
    let mut child = command
        .spawn()
        .map_err(|error| McpError::Transport(error.to_string()))?;
    let mut stdin = child.stdin.take().ok_or(McpError::Protocol)?;
    stdin.write_all(request)?;
    stdin.write_all(b"\n")?;
    drop(stdin);
    let stdout = child.stdout.take().ok_or(McpError::Protocol)?;
    let (sender, receiver) = mpsc::sync_channel(1);
    thread::spawn(move || {
        let mut response = Vec::new();
        let result = BufReader::new(stdout)
            .take(
                u64::try_from(max_response_bytes)
                    .unwrap_or(u64::MAX)
                    .saturating_add(1),
            )
            .read_until(b'\n', &mut response)
            .map(|_| response);
        let _ = sender.send(result);
    });
    let response = match receiver.recv_timeout(Duration::from_millis(timeout_ms)) {
        Ok(result) => result?,
        Err(mpsc::RecvTimeoutError::Timeout) => {
            terminate_process_group(&mut child);
            let _ = child.wait();
            return Err(McpError::Timeout);
        }
        Err(mpsc::RecvTimeoutError::Disconnected) => return Err(McpError::Protocol),
    };
    terminate_process_group(&mut child);
    let _ = child.wait();
    if response.len() > max_response_bytes {
        return Err(McpError::SizeLimit);
    }
    Ok(response)
}

#[cfg(unix)]
fn configure_process_group(command: &mut Command) {
    command.process_group(0);
}

#[cfg(windows)]
fn configure_process_group(command: &mut Command) {
    const CREATE_NEW_PROCESS_GROUP: u32 = 0x0000_0200;
    command.creation_flags(CREATE_NEW_PROCESS_GROUP);
}

#[cfg(not(any(unix, windows)))]
fn configure_process_group(_command: &mut Command) {}

#[cfg(unix)]
fn terminate_process_group(child: &mut std::process::Child) {
    if let Ok(pid) = i32::try_from(child.id()) {
        let _ = killpg(Pid::from_raw(pid), Signal::SIGKILL);
    } else {
        let _ = child.kill();
    }
}

#[cfg(windows)]
fn terminate_process_group(child: &mut std::process::Child) {
    let status = Command::new("taskkill")
        .args(["/PID", &child.id().to_string(), "/T", "/F"])
        .status();
    if !status.is_ok_and(|status| status.success()) {
        let _ = child.kill();
    }
}

#[cfg(not(any(unix, windows)))]
fn terminate_process_group(child: &mut std::process::Child) {
    let _ = child.kill();
}

#[allow(clippy::too_many_arguments)]
fn http_request(
    endpoint: &str,
    headers: &BTreeMap<String, String>,
    credential: Option<&McpCredential>,
    secret: Option<&str>,
    request: &[u8],
    timeout_ms: u64,
    max_response_bytes: usize,
) -> Result<Vec<u8>, McpError> {
    let url = Url::parse(endpoint).map_err(|_| McpError::InvalidConfiguration)?;
    let host = url.host_str().ok_or(McpError::InvalidConfiguration)?;
    let port = url
        .port_or_known_default()
        .ok_or(McpError::InvalidConfiguration)?;
    let mut stream =
        TcpStream::connect((host, port)).map_err(|error| McpError::Transport(error.to_string()))?;
    let timeout = Some(Duration::from_millis(timeout_ms));
    stream.set_read_timeout(timeout)?;
    stream.set_write_timeout(timeout)?;
    let path = if url.path().is_empty() {
        "/"
    } else {
        url.path()
    };
    let mut head = format!(
        "POST {path} HTTP/1.1\r\nHost: {host}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n",
        request.len()
    );
    for (name, value) in headers {
        write!(&mut head, "{name}: {value}\r\n").expect("writing to String cannot fail");
    }
    if let (
        Some(McpCredential {
            placement: McpAuthentication::Header(name),
            ..
        }),
        Some(value),
    ) = (credential, secret)
    {
        write!(&mut head, "{name}: {value}\r\n").expect("writing to String cannot fail");
    }
    head.push_str("\r\n");
    stream.write_all(head.as_bytes())?;
    stream.write_all(request)?;
    let mut response = Vec::new();
    stream
        .take(
            u64::try_from(max_response_bytes)
                .unwrap_or(u64::MAX)
                .saturating_add(8_192),
        )
        .read_to_end(&mut response)
        .map_err(|error| {
            if matches!(
                error.kind(),
                std::io::ErrorKind::TimedOut | std::io::ErrorKind::WouldBlock
            ) {
                McpError::Timeout
            } else {
                McpError::Io(error)
            }
        })?;
    let separator = response
        .windows(4)
        .position(|window| window == b"\r\n\r\n")
        .ok_or(McpError::Protocol)?;
    let status = response
        .get(..separator)
        .and_then(|head| std::str::from_utf8(head).ok())
        .and_then(|head| head.lines().next())
        .ok_or(McpError::Protocol)?;
    if status.contains(" 401 ") || status.contains(" 403 ") {
        return Err(McpError::Authentication);
    }
    if !status.contains(" 200 ") {
        return Err(McpError::Transport(
            "HTTP server rejected request".to_owned(),
        ));
    }
    let body = response.split_off(separator + 4);
    if body.len() > max_response_bytes {
        return Err(McpError::SizeLimit);
    }
    Ok(body)
}

fn required_string(value: &Value, name: &str) -> Result<String, McpError> {
    value
        .get(name)
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty())
        .map(str::to_owned)
        .ok_or(McpError::Protocol)
}

fn digest_json(value: &impl Serialize) -> Result<String, McpError> {
    let mut digest = Sha256::new();
    digest.update(serde_json::to_vec(value)?);
    Ok(format!("{:x}", digest.finalize()))
}

fn terms(value: &str) -> BTreeSet<String> {
    value
        .split(|character: char| !character.is_alphanumeric())
        .filter(|term| !term.is_empty())
        .map(str::to_lowercase)
        .collect()
}

fn dot_product(left: &[i32], right: &[i32]) -> i64 {
    left.iter()
        .zip(right)
        .map(|(left, right)| i64::from(*left).saturating_mul(i64::from(*right)))
        .fold(0_i64, i64::saturating_add)
}

#[cfg(test)]
mod tests {
    use std::net::TcpListener;
    #[cfg(unix)]
    use std::os::unix::fs::PermissionsExt;

    use keith_credentials::{MasterKey, SecretValue};
    use tempfile::TempDir;

    use super::*;

    fn credential_store(root: &Path) -> EncryptedCredentialStore {
        EncryptedCredentialStore::open(root, MasterKey::from_bytes([9; 32]))
            .expect("credential store")
    }

    fn server_config(id: &str, profile: &ProfileId, transport: McpTransport) -> McpServerConfig {
        McpServerConfig {
            id: id.to_owned(),
            transport,
            enabled_profiles: BTreeSet::from([profile.clone()]),
            credential: None,
            allowed_filesystem_roots: Vec::new(),
            allowed_network_hosts: BTreeSet::new(),
            timeout_ms: 1_000,
            max_request_bytes: 4_096,
            max_response_bytes: 8_192,
            max_tools: 8,
        }
    }

    #[cfg(unix)]
    #[test]
    fn stdio_schema_relevance_calls_timeout_schema_change_isolation_and_cleanup() {
        let root = TempDir::new().expect("manager root");
        let credentials_root = TempDir::new().expect("credential root");
        let scripts = TempDir::new().expect("scripts");
        let script = scripts.path().join("mcp-server.sh");
        let schema_marker = scripts.path().join("schema-v2");
        fs::write(
            &script,
            r#"#!/bin/sh
read request
case "$request" in
  *tools/list*) if [ -f "$1" ]; then
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"calendar_lookup","description":"find calendar events","inputSchema":{},"embedding":[3,0]},{"name":"repo_search","description":"search source repository","inputSchema":{},"embedding":[0,3]}]}}'
    else
      : > "$1"
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"calendar_lookup","description":"find calendar events","inputSchema":{},"embedding":[3,0]}]}}'
    fi ;;
  *tools/call*) printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"done"}],"isError":false}}' ;;
esac
"#,
        )
        .expect("server script");
        fs::set_permissions(&script, fs::Permissions::from_mode(0o700)).expect("executable");
        let credentials = Arc::new(credential_store(credentials_root.path()));
        let profile = ProfileId::new();
        let denied_profile = ProfileId::new();
        let mut manager =
            McpManager::open(root.path(), Arc::clone(&credentials), 1).expect("manager");
        manager
            .configure(server_config(
                "stdio",
                &profile,
                McpTransport::Stdio {
                    executable: script,
                    args: vec![schema_marker.to_string_lossy().into_owned()],
                    working_directory: None,
                    environment: BTreeMap::new(),
                },
            ))
            .expect("configure");
        assert_eq!(
            manager
                .refresh_schema("stdio", UtcTimestamp::UNIX_EPOCH)
                .expect("schema")
                .version,
            1
        );
        assert_eq!(
            manager
                .refresh_schema("stdio", UtcTimestamp::from_unix_millis(1))
                .expect("changed schema")
                .version,
            2
        );
        let selected = manager.relevant_tools(&profile, "calendar", &[3, 0], 1_024);
        assert_eq!(selected.len(), 1);
        assert_eq!(selected[0].name, "calendar_lookup");
        assert!(
            manager
                .relevant_tools(&denied_profile, "calendar", &[3, 0], 1_024)
                .is_empty()
        );
        let session = SessionId::new();
        manager
            .open_session(&session, profile.clone(), "stdio")
            .expect("session");
        assert!(matches!(
            manager.open_session(&SessionId::new(), profile, "stdio"),
            Err(McpError::SessionLimit)
        ));
        assert_eq!(
            manager
                .call_tool(&session, "stdio", "calendar_lookup", &json!({}))
                .expect("tool")
                .content[0]["text"],
            "done"
        );
        assert_eq!(manager.cleanup_sessions(), 1);
        assert!(matches!(
            manager.call_tool(&session, "stdio", "calendar_lookup", &json!({})),
            Err(McpError::NotFound)
        ));
        drop(manager);
        let reopened =
            McpManager::open(root.path(), Arc::clone(&credentials), 1).expect("restart manager");
        assert_eq!(reopened.cache("stdio").expect("durable cache").version, 2);
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn http_auth_reconnect_malicious_output_and_profile_bounds_are_enforced() {
        let root = TempDir::new().expect("manager root");
        let credentials_root = TempDir::new().expect("credential root");
        let credentials = Arc::new(credential_store(credentials_root.path()));
        let profile = ProfileId::new();
        let listener = TcpListener::bind("127.0.0.1:0").expect("HTTP listener");
        let address = listener.local_addr().expect("address");
        let server = thread::spawn(move || {
            for _ in 0..2 {
                let (mut stream, _) = listener.accept().expect("HTTP connection");
                let mut reader = BufReader::new(stream.try_clone().expect("clone HTTP stream"));
                let mut request = String::new();
                loop {
                    let mut line = String::new();
                    reader.read_line(&mut line).expect("HTTP header line");
                    if line == "\r\n" || line.is_empty() {
                        break;
                    }
                    request.push_str(&line);
                }
                let content_length = request
                    .lines()
                    .find_map(|line| {
                        let (name, value) = line.split_once(':')?;
                        name.eq_ignore_ascii_case("content-length")
                            .then(|| value.trim().parse::<usize>().ok())?
                    })
                    .unwrap_or(0);
                let mut request_body = vec![0_u8; content_length];
                reader
                    .read_exact(&mut request_body)
                    .expect("complete HTTP request body");
                let (status, body) = if request.contains("Authorization: secret") {
                    (
                        "200 OK",
                        r#"{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"web_search","description":"search web","inputSchema":{},"embedding":[1]}]}}"#,
                    )
                } else {
                    ("401 Unauthorized", "{}")
                };
                write!(
                    stream,
                    "HTTP/1.1 {status}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                    body.len()
                )
                .expect("HTTP response");
            }
        });
        let reference = CredentialRef::new("token", CredentialOwner::Mcp("http".to_owned()))
            .expect("credential reference");
        let mut config = server_config(
            "http",
            &profile,
            McpTransport::Http {
                endpoint: format!("http://{address}/mcp"),
                headers: BTreeMap::new(),
            },
        );
        config.allowed_network_hosts.insert("127.0.0.1".to_owned());
        config.credential = Some(McpCredential {
            reference: reference.clone(),
            placement: McpAuthentication::Header("Authorization".to_owned()),
        });
        credentials
            .put(
                reference.clone(),
                SecretValue::new(b"wrong".to_vec()).expect("wrong secret"),
                UtcTimestamp::UNIX_EPOCH,
            )
            .expect("store wrong credential");
        let mut manager =
            McpManager::open(root.path(), Arc::clone(&credentials), 2).expect("manager");
        manager.configure(config).expect("configure");
        let authentication = manager.refresh_schema("http", UtcTimestamp::UNIX_EPOCH);
        assert!(
            matches!(authentication, Err(McpError::Authentication)),
            "unexpected authentication result: {authentication:?}"
        );
        credentials
            .put(
                reference,
                SecretValue::new(b"secret".to_vec()).expect("secret"),
                UtcTimestamp::from_unix_millis(1),
            )
            .expect("store credential");
        manager
            .reconnect("http", UtcTimestamp::from_unix_millis(1))
            .expect("authenticated reconnect");
        assert_eq!(manager.status("http").expect("status").generation, 1);
        server.join().expect("HTTP server");

        let invalid = server_config(
            "invalid",
            &profile,
            McpTransport::Http {
                endpoint: "http://example.com/mcp".to_owned(),
                headers: BTreeMap::new(),
            },
        );
        assert!(matches!(
            manager.configure(invalid),
            Err(McpError::InvalidConfiguration)
        ));
    }

    #[cfg(unix)]
    #[test]
    fn timeout_and_malicious_output_are_bounded_and_processes_are_reaped() {
        let root = TempDir::new().expect("manager root");
        let credentials_root = TempDir::new().expect("credential root");
        let scripts = TempDir::new().expect("scripts");
        let credentials = Arc::new(credential_store(credentials_root.path()));
        let profile = ProfileId::new();
        for (id, body, expected) in [
            ("slow", "sleep 2", McpError::Timeout),
            (
                "flood",
                "head -c 10000 /dev/zero | tr '\\0' x; printf '\\n'",
                McpError::SizeLimit,
            ),
        ] {
            let script = scripts.path().join(format!("{id}.sh"));
            fs::write(&script, format!("#!/bin/sh\nread request\n{body}\n")).expect("script");
            fs::set_permissions(&script, fs::Permissions::from_mode(0o700)).expect("executable");
            let mut config = server_config(
                id,
                &profile,
                McpTransport::Stdio {
                    executable: script,
                    args: Vec::new(),
                    working_directory: None,
                    environment: BTreeMap::new(),
                },
            );
            config.timeout_ms = 30;
            config.max_response_bytes = 128;
            let mut manager =
                McpManager::open(root.path(), Arc::clone(&credentials), 2).expect("manager");
            manager.configure(config).expect("configure");
            let error = manager
                .refresh_schema(id, UtcTimestamp::UNIX_EPOCH)
                .expect_err("bounded failure");
            assert_eq!(
                std::mem::discriminant(&error),
                std::mem::discriminant(&expected)
            );
        }
    }
}
