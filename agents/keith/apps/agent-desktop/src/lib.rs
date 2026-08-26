#![forbid(unsafe_code)]

use std::collections::BTreeSet;
use std::fmt::Write as _;
use std::fs::{self, File, OpenOptions};
use std::io::{Read, Write};
use std::path::{Component, Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::thread;
use std::time::{Duration, Instant};

use keith_agent_types::{
    CURRENT_PROTOCOL_VERSION, CURRENT_SCHEMA_VERSION, ClientId, CommandId, EntityId,
    ProtocolVersion, SchemaVersion, UtcTimestamp,
};
use keith_connection::{
    AgentTransport, FramedTransport, connect_local, set_local_read_timeout, set_local_write_timeout,
};
use keith_platform::PlatformPaths;
use keith_protocol::{
    AgentLifecycleCommand, AgentRosterProjection, ClientCommand, ClientHello, CommandEnvelope,
    CommandResult, CommandResultEnvelope, Feature, ResponsePayload, SessionFilter, SessionSummary,
    WireFormat, WireMessage,
};
use keith_release::{
    ReleaseError, decode_public_key, verify_packaged_build_reports, verify_release,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;
use url::Url;

const MAX_CRASH_BYTES: usize = 64 * 1_024;
const MAX_NOTIFICATION_BYTES: usize = 8 * 1_024;

#[derive(Debug, Error)]
pub enum DesktopError {
    #[error("desktop I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("desktop state encoding failed: {0}")]
    Json(#[from] serde_json::Error),
    #[error("desktop configuration is invalid")]
    InvalidConfiguration,
    #[error("desktop path is unsafe")]
    UnsafePath,
    #[error("desktop process did not become ready")]
    StartupTimeout,
    #[error("desktop process is not owned by this shell")]
    NotOwned,
    #[error("desktop endpoint is already served by an unverified process")]
    ExistingProcess,
    #[error("agent connection failed: {0}")]
    AgentConnection(String),
    #[error("signed release verification failed: {0}")]
    Release(#[from] ReleaseError),
    #[error("update version already exists")]
    VersionExists,
    #[error("update rollback target is unavailable")]
    RollbackUnavailable,
    #[error("uninstall confirmation did not match the exact scope")]
    ConfirmationRequired,
    #[error("browser handoff is not a local authenticated application route")]
    InvalidBrowserHandoff,
    #[error("required child secret environment is unavailable")]
    MissingSecretEnvironment,
    #[error("random generation failed")]
    Random,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DesktopSettings {
    pub version: SchemaVersion,
    pub installation_id: EntityId,
    pub state_root: PathBuf,
    pub data_root: PathBuf,
    pub daemon_socket: PathBuf,
    pub web_origin: String,
    pub created_at: UtcTimestamp,
}

pub struct DesktopBootstrap;

impl DesktopBootstrap {
    /// Loads and validates existing desktop settings from an explicit state root.
    ///
    /// # Errors
    ///
    /// Returns an error when the settings are missing, malformed, unsafe, or belong to another
    /// state root.
    pub fn load(state_root: &Path) -> Result<DesktopSettings, DesktopError> {
        validate_absolute_root(state_root)?;
        reject_symlink(state_root)?;
        let settings =
            serde_json::from_slice::<DesktopSettings>(&fs::read(state_root.join("desktop.json"))?)?;
        validate_absolute_root(&settings.state_root)?;
        validate_absolute_root(&settings.data_root)?;
        validate_loopback_origin(&settings.web_origin)?;
        if settings.version.major != CURRENT_SCHEMA_VERSION.major
            || settings.state_root != state_root
            || !settings.daemon_socket.starts_with(&settings.data_root)
        {
            return Err(DesktopError::InvalidConfiguration);
        }
        Ok(settings)
    }

    /// Creates or reopens desktop state in the native platform locations.
    ///
    /// # Errors
    ///
    /// Returns an error when platform paths cannot be discovered or initialized.
    pub fn initialize_default(web_origin: &str) -> Result<DesktopSettings, DesktopError> {
        let paths = PlatformPaths::discover().map_err(|_| DesktopError::InvalidConfiguration)?;
        let mut settings = Self::initialize(&paths.state_root, &paths.data_root, web_origin)?;
        if settings.daemon_socket != paths.daemon_endpoint {
            settings.daemon_socket = paths.daemon_endpoint;
            atomic_json(&settings.state_root.join("desktop.json"), &settings)?;
        }
        Ok(settings)
    }

    /// Creates or reopens the non-secret first-run desktop state.
    ///
    /// # Errors
    ///
    /// Returns an error for unsafe paths, invalid loopback origin, or persistence failure.
    pub fn initialize(
        state_root: &Path,
        data_root: &Path,
        web_origin: &str,
    ) -> Result<DesktopSettings, DesktopError> {
        validate_absolute_root(state_root)?;
        validate_absolute_root(data_root)?;
        validate_loopback_origin(web_origin)?;
        fs::create_dir_all(state_root)?;
        fs::create_dir_all(data_root)?;
        for directory in ["crashes", "notifications", "updates", "backups"] {
            fs::create_dir_all(state_root.join(directory))?;
        }
        let path = state_root.join("desktop.json");
        if path.exists() {
            let existing = serde_json::from_slice::<DesktopSettings>(&fs::read(path)?)?;
            if existing.version.major != CURRENT_SCHEMA_VERSION.major
                || existing.state_root != state_root
                || existing.data_root != data_root
                || existing.web_origin != web_origin
            {
                return Err(DesktopError::InvalidConfiguration);
            }
            return Ok(existing);
        }
        let settings = DesktopSettings {
            version: CURRENT_SCHEMA_VERSION,
            installation_id: EntityId::new(),
            state_root: state_root.to_path_buf(),
            data_root: data_root.to_path_buf(),
            daemon_socket: data_root.join("agentd.sock"),
            web_origin: web_origin.into(),
            created_at: UtcTimestamp::now().map_err(|_| DesktopError::InvalidConfiguration)?,
        };
        atomic_json(&path, &settings)?;
        Ok(settings)
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DesktopProcessConfig {
    pub settings: DesktopSettings,
    pub workspace_root: PathBuf,
    pub daemon_executable: PathBuf,
    pub worker_executable: PathBuf,
    pub web_executable: PathBuf,
    pub web_bind: String,
    pub asset_root: PathBuf,
    pub credential_root: PathBuf,
    pub login_secret_env: String,
    pub credential_key_env: String,
    pub reuse_existing_processes: bool,
    pub startup_timeout: Duration,
    pub shutdown_grace: Duration,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ProcessOwnership {
    Existing,
    Owned,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ManagedProcessKind {
    Daemon,
    Web,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CrashReport {
    pub version: SchemaVersion,
    pub id: EntityId,
    pub process: ManagedProcessKind,
    pub exit_code: Option<i32>,
    pub safe_detail: String,
    pub observed_at: UtcTimestamp,
}

struct OwnedProcess {
    child: Child,
    stderr_path: PathBuf,
}

pub struct DesktopLifecycle {
    config: DesktopProcessConfig,
    daemon: Option<OwnedProcess>,
    web: Option<OwnedProcess>,
}

impl DesktopLifecycle {
    /// Validates packaged executables and web assets before process ownership begins.
    ///
    /// # Errors
    ///
    /// Returns an error when paths, timeouts, executables, assets, or origins are invalid.
    pub fn new(config: DesktopProcessConfig) -> Result<Self, DesktopError> {
        if config.startup_timeout.is_zero()
            || config.shutdown_grace.is_zero()
            || !config.daemon_executable.is_file()
            || !config.worker_executable.is_file()
            || !config.web_executable.is_file()
            || !config.asset_root.join("agent_web.js").is_file()
            || !config.asset_root.join("agent_web_bg.wasm").is_file()
            || !valid_production_web_assets(&config.asset_root)
            || !config.workspace_root.is_dir()
            || config.login_secret_env.is_empty()
            || config.credential_key_env.is_empty()
        {
            return Err(DesktopError::InvalidConfiguration);
        }
        validate_loopback_origin(&config.settings.web_origin)?;
        let bind = config
            .web_bind
            .parse::<std::net::SocketAddr>()
            .map_err(|_| DesktopError::InvalidConfiguration)?;
        let origin = Url::parse(&config.settings.web_origin)
            .map_err(|_| DesktopError::InvalidConfiguration)?;
        if !bind.ip().is_loopback() || origin.port_or_known_default() != Some(bind.port()) {
            return Err(DesktopError::InvalidConfiguration);
        }
        Ok(Self {
            config,
            daemon: None,
            web: None,
        })
    }

    /// Reuses a healthy daemon or starts and probes the packaged daemon process.
    ///
    /// # Errors
    ///
    /// Returns an error for unsafe stale endpoints, spawn failure, or readiness timeout.
    pub fn ensure_daemon(&mut self) -> Result<ProcessOwnership, DesktopError> {
        if DesktopConnection::probe(&self.config.settings.daemon_socket).is_ok() {
            return if self.config.reuse_existing_processes {
                Ok(ProcessOwnership::Existing)
            } else {
                Err(DesktopError::ExistingProcess)
            };
        }
        remove_stale_socket(&self.config.settings.daemon_socket)?;
        let stderr_path = self.process_log_path(ManagedProcessKind::Daemon);
        let stderr = OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(&stderr_path)?;
        let child = Command::new(&self.config.daemon_executable)
            .arg("--data-root")
            .arg(&self.config.settings.data_root)
            .arg("--socket")
            .arg(&self.config.settings.daemon_socket)
            .arg("--worker-executable")
            .arg(&self.config.worker_executable)
            .arg("--credential-root")
            .arg(&self.config.credential_root)
            .arg("--credential-key-env")
            .arg(&self.config.credential_key_env)
            .arg("--workspace-root")
            .arg(&self.config.workspace_root)
            .arg("--idle-seconds")
            .arg("900")
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::from(stderr))
            .spawn()?;
        self.daemon = Some(OwnedProcess { child, stderr_path });
        self.wait_for_daemon()?;
        Ok(ProcessOwnership::Owned)
    }

    /// Starts the authenticated packaged web application or reuses its listener.
    ///
    /// # Errors
    ///
    /// Returns an error for missing narrow secret references, spawn failure, or timeout.
    pub fn ensure_web(&mut self) -> Result<ProcessOwnership, DesktopError> {
        if web_listener_ready(&self.config.web_bind) {
            return if self.config.reuse_existing_processes {
                Ok(ProcessOwnership::Existing)
            } else {
                Err(DesktopError::ExistingProcess)
            };
        }
        let login_secret = std::env::var_os(&self.config.login_secret_env)
            .ok_or(DesktopError::MissingSecretEnvironment)?;
        let credential_key = std::env::var_os(&self.config.credential_key_env)
            .ok_or(DesktopError::MissingSecretEnvironment)?;
        let stderr_path = self.process_log_path(ManagedProcessKind::Web);
        let stderr = OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(&stderr_path)?;
        let mut command = Command::new(&self.config.web_executable);
        command
            .arg("--bind")
            .arg(&self.config.web_bind)
            .arg("--origin")
            .arg(&self.config.settings.web_origin)
            .arg("--socket")
            .arg(&self.config.settings.daemon_socket)
            .arg("--asset-root")
            .arg(&self.config.asset_root)
            .arg("--credential-root")
            .arg(&self.config.credential_root)
            .arg("--login-secret-env")
            .arg(&self.config.login_secret_env)
            .arg("--credential-key-env")
            .arg(&self.config.credential_key_env)
            .env_clear()
            .env(&self.config.login_secret_env, login_secret)
            .env(&self.config.credential_key_env, credential_key)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::from(stderr));
        let child = command.spawn()?;
        self.web = Some(OwnedProcess { child, stderr_path });
        self.wait_for_web()?;
        Ok(ProcessOwnership::Owned)
    }

    pub fn connection(&self) -> DesktopConnection {
        DesktopConnection {
            socket: self.config.settings.daemon_socket.clone(),
        }
    }

    /// Observes real child termination and persists a bounded redacted crash report.
    ///
    /// # Errors
    ///
    /// Returns an error when process status or crash persistence cannot be inspected.
    pub fn poll_crashes(&mut self) -> Result<Vec<CrashReport>, DesktopError> {
        let mut reports = Vec::new();
        if let Some(report) = poll_process(
            &self.config.settings.state_root,
            &mut self.daemon,
            ManagedProcessKind::Daemon,
        )? {
            reports.push(report);
        }
        if let Some(report) = poll_process(
            &self.config.settings.state_root,
            &mut self.web,
            ManagedProcessKind::Web,
        )? {
            reports.push(report);
        }
        Ok(reports)
    }

    /// Gracefully stops only processes started by this desktop instance.
    ///
    /// # Errors
    ///
    /// Returns an error if a child cannot be signalled, reaped, or force-stopped.
    pub fn stop_owned(&mut self) -> Result<(), DesktopError> {
        stop_process(&mut self.web, self.config.shutdown_grace)?;
        stop_process(&mut self.daemon, self.config.shutdown_grace)
    }

    fn wait_for_daemon(&mut self) -> Result<(), DesktopError> {
        let deadline = Instant::now() + self.config.startup_timeout;
        loop {
            if DesktopConnection::probe(&self.config.settings.daemon_socket).is_ok() {
                return Ok(());
            }
            if self
                .daemon
                .as_mut()
                .and_then(|process| process.child.try_wait().ok().flatten())
                .is_some()
            {
                return Err(DesktopError::StartupTimeout);
            }
            if Instant::now() >= deadline {
                stop_process(&mut self.daemon, self.config.shutdown_grace)?;
                return Err(DesktopError::StartupTimeout);
            }
            thread::sleep(Duration::from_millis(10));
        }
    }

    fn wait_for_web(&mut self) -> Result<(), DesktopError> {
        let deadline = Instant::now() + self.config.startup_timeout;
        loop {
            if web_listener_ready(&self.config.web_bind) {
                return Ok(());
            }
            if self
                .web
                .as_mut()
                .and_then(|process| process.child.try_wait().ok().flatten())
                .is_some()
            {
                return Err(DesktopError::StartupTimeout);
            }
            if Instant::now() >= deadline {
                stop_process(&mut self.web, self.config.shutdown_grace)?;
                return Err(DesktopError::StartupTimeout);
            }
            thread::sleep(Duration::from_millis(10));
        }
    }

    fn process_log_path(&self, kind: ManagedProcessKind) -> PathBuf {
        let label = match kind {
            ManagedProcessKind::Daemon => "daemon",
            ManagedProcessKind::Web => "web",
        };
        self.config
            .settings
            .state_root
            .join("crashes")
            .join(format!("{label}-{}.stderr", EntityId::new()))
    }
}

fn valid_production_web_assets(root: &Path) -> bool {
    let manifest_path = root.join("ui/.vite/manifest.json");
    let Ok(encoded) = fs::read(manifest_path) else {
        return false;
    };
    let Ok(manifest) = serde_json::from_slice::<serde_json::Value>(&encoded) else {
        return false;
    };
    let Some(entry) = manifest.get("src/index.tsx") else {
        return false;
    };
    let Some(script) = entry.get("file").and_then(serde_json::Value::as_str) else {
        return false;
    };
    let Some(styles) = entry.get("css").and_then(serde_json::Value::as_array) else {
        return false;
    };
    entry.get("isEntry").and_then(serde_json::Value::as_bool) == Some(true)
        && safe_relative_asset(script)
        && root.join("ui").join(script).is_file()
        && !styles.is_empty()
        && styles.iter().all(|style| {
            style.as_str().is_some_and(|path| {
                safe_relative_asset(path) && root.join("ui").join(path).is_file()
            })
        })
}

fn safe_relative_asset(value: &str) -> bool {
    let path = Path::new(value);
    !path.as_os_str().is_empty()
        && path
            .components()
            .all(|component| matches!(component, Component::Normal(_)))
}

impl Drop for DesktopLifecycle {
    fn drop(&mut self) {
        let _ = self.stop_owned();
    }
}

#[derive(Clone, Debug)]
pub struct DesktopConnection {
    socket: PathBuf,
}

impl DesktopConnection {
    /// Lists sessions exclusively through `AgentConnection`.
    ///
    /// # Errors
    ///
    /// Returns an error for transport, negotiation, or response-type failure.
    pub fn list_sessions(&self) -> Result<Vec<SessionSummary>, DesktopError> {
        let mut client = self.connect()?;
        let result = client.execute(ClientCommand::ListSessions(SessionFilter::default()))?;
        let CommandResult::Data(payload) = result.result else {
            return Err(DesktopError::AgentConnection(
                "daemon rejected session listing".into(),
            ));
        };
        let ResponsePayload::Sessions(sessions) = *payload else {
            return Err(DesktopError::AgentConnection(
                "daemon returned an unexpected response".into(),
            ));
        };
        Ok(sessions)
    }

    /// Lists the authoritative persistent-agent roster through the same native
    /// transport used by the web client.
    ///
    /// # Errors
    ///
    /// Returns an error for negotiation, correlation, or response-type failure.
    pub fn list_roster(&self) -> Result<Vec<AgentRosterProjection>, DesktopError> {
        let mut client = self.connect()?;
        let result = client.execute(ClientCommand::AgentLifecycle(AgentLifecycleCommand::List))?;
        let CommandResult::Data(payload) = result.result else {
            return Err(DesktopError::AgentConnection(
                "daemon rejected roster listing".into(),
            ));
        };
        let ResponsePayload::AgentRoster(roster) = *payload else {
            return Err(DesktopError::AgentConnection(
                "daemon returned an unexpected response".into(),
            ));
        };
        Ok(roster)
    }

    fn connect(&self) -> Result<DesktopNativeClient, DesktopError> {
        let stream = connect_local(&self.socket)
            .map_err(|error| DesktopError::AgentConnection(error.to_string()))?;
        set_local_read_timeout(&stream, Some(Duration::from_secs(2)))
            .map_err(|error| DesktopError::AgentConnection(error.to_string()))?;
        set_local_write_timeout(&stream, Some(Duration::from_secs(2)))
            .map_err(|error| DesktopError::AgentConnection(error.to_string()))?;
        let mut transport = FramedTransport::new(stream, WireFormat::Json);
        let client_id = ClientId::new();
        transport
            .send(&WireMessage::ClientHello(ClientHello {
                protocol: CURRENT_PROTOCOL_VERSION,
                client_id: client_id.clone(),
                client_name: "keith-agent-desktop".into(),
                client_version: env!("CARGO_PKG_VERSION").into(),
                supported_features: BTreeSet::from([
                    Feature::SessionLifecycle,
                    Feature::Replay,
                    Feature::Snapshots,
                    Feature::FramedJson,
                    Feature::AgentLifecycle,
                    Feature::Conversations,
                ]),
                resume: None,
            }))
            .map_err(agent_connection_error)?;
        let WireMessage::ServerHello(hello) =
            transport.receive().map_err(agent_connection_error)?
        else {
            return Err(DesktopError::AgentConnection(
                "daemon did not negotiate AgentConnection".into(),
            ));
        };
        if !hello
            .protocol
            .is_major_compatible_with(CURRENT_PROTOCOL_VERSION)
            || !hello.supported_features.contains(&Feature::AgentLifecycle)
            || !hello.supported_features.contains(&Feature::Conversations)
        {
            return Err(DesktopError::AgentConnection(
                "daemon lacks required native desktop features".into(),
            ));
        }
        Ok(DesktopNativeClient {
            transport,
            client_id,
            protocol: hello.protocol,
        })
    }

    fn probe(socket: &Path) -> Result<(), DesktopError> {
        Self {
            socket: socket.to_path_buf(),
        }
        .list_sessions()
        .map(|_| ())
    }
}

struct DesktopNativeClient {
    transport: FramedTransport<keith_connection::LocalStream>,
    client_id: ClientId,
    protocol: ProtocolVersion,
}

impl DesktopNativeClient {
    fn execute(&mut self, command: ClientCommand) -> Result<CommandResultEnvelope, DesktopError> {
        let command_id = CommandId::new();
        self.transport
            .send(&WireMessage::Command(CommandEnvelope {
                protocol: self.protocol,
                command_id: command_id.clone(),
                client_id: self.client_id.clone(),
                sent_at: UtcTimestamp::now().map_err(|_| DesktopError::InvalidConfiguration)?,
                session_id: None,
                command,
            }))
            .map_err(agent_connection_error)?;
        let WireMessage::CommandResult(result) =
            self.transport.receive().map_err(agent_connection_error)?
        else {
            return Err(DesktopError::AgentConnection(
                "daemon returned an unexpected message".into(),
            ));
        };
        if result.protocol != self.protocol || result.command_id != command_id {
            return Err(DesktopError::AgentConnection(
                "daemon returned an uncorrelated response".into(),
            ));
        }
        Ok(result)
    }
}

fn agent_connection_error(error: keith_connection::ConnectionError) -> DesktopError {
    DesktopError::AgentConnection(error.to_string())
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DesktopNotification {
    pub version: SchemaVersion,
    pub id: EntityId,
    pub title: String,
    pub body: String,
    pub route: String,
    pub created_at: UtcTimestamp,
    pub acknowledged_at: Option<UtcTimestamp>,
}

pub struct DesktopNotificationCenter {
    root: PathBuf,
}

impl DesktopNotificationCenter {
    /// Opens the in-shell local notification queue.
    ///
    /// # Errors
    ///
    /// Returns an error for an unsafe or unavailable notification root.
    pub fn open(state_root: &Path) -> Result<Self, DesktopError> {
        let root = state_root.join("notifications");
        fs::create_dir_all(&root)?;
        reject_symlink(&root)?;
        Ok(Self { root })
    }

    /// Persists a bounded local notification linked to an application route.
    ///
    /// # Errors
    ///
    /// Returns an error for oversized/private content, invalid route, or persistence failure.
    pub fn notify(
        &self,
        title: impl Into<String>,
        body: impl Into<String>,
        route: impl Into<String>,
        now: UtcTimestamp,
    ) -> Result<DesktopNotification, DesktopError> {
        let notification = DesktopNotification {
            version: CURRENT_SCHEMA_VERSION,
            id: EntityId::new(),
            title: title.into(),
            body: body.into(),
            route: route.into(),
            created_at: now,
            acknowledged_at: None,
        };
        if notification.title.trim().is_empty()
            || notification.title.len() + notification.body.len() > MAX_NOTIFICATION_BYTES
            || !notification.route.starts_with('/')
            || notification.route.starts_with("//")
            || contains_secret(&format!("{} {}", notification.title, notification.body))
        {
            return Err(DesktopError::InvalidConfiguration);
        }
        atomic_json(&self.path(&notification.id), &notification)?;
        Ok(notification)
    }

    /// Marks a persisted notification as observed by the local shell.
    ///
    /// # Errors
    ///
    /// Returns an error when the notification is missing or cannot be persisted.
    pub fn acknowledge(
        &self,
        id: &EntityId,
        now: UtcTimestamp,
    ) -> Result<DesktopNotification, DesktopError> {
        let path = self.path(id);
        let mut notification = serde_json::from_slice::<DesktopNotification>(&fs::read(&path)?)?;
        notification.acknowledged_at = Some(now);
        atomic_json(&path, &notification)?;
        Ok(notification)
    }

    fn path(&self, id: &EntityId) -> PathBuf {
        self.root.join(format!("{id}.json"))
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SelectedFile {
    pub path: PathBuf,
    pub bytes: u64,
}

/// Resolves a file selection beneath an explicitly granted root without reading its content.
///
/// # Errors
///
/// Returns an error for symlinks, directories, missing files, or paths outside every root.
pub fn select_file(path: &Path, allowed_roots: &[PathBuf]) -> Result<SelectedFile, DesktopError> {
    reject_symlink(path)?;
    let canonical = fs::canonicalize(path)?;
    let allowed = allowed_roots.iter().any(|root| {
        fs::canonicalize(root).is_ok_and(|canonical_root| canonical.starts_with(canonical_root))
    });
    let metadata = fs::metadata(&canonical)?;
    if !allowed || !metadata.is_file() {
        return Err(DesktopError::UnsafePath);
    }
    Ok(SelectedFile {
        path: canonical,
        bytes: metadata.len(),
    })
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct BrowserHandoff {
    url: Url,
}

impl BrowserHandoff {
    /// Constructs a loopback browser route without credentials or fragments.
    ///
    /// # Errors
    ///
    /// Returns an error for non-loopback origins, absolute route URLs, queries, or fragments.
    pub fn new(origin: &str, route: &str) -> Result<Self, DesktopError> {
        validate_loopback_origin(origin)?;
        if !route.starts_with('/')
            || route.starts_with("//")
            || route.contains(['?', '#'])
            || route.contains("..")
        {
            return Err(DesktopError::InvalidBrowserHandoff);
        }
        let url = Url::parse(origin)
            .and_then(|base| base.join(route))
            .map_err(|_| DesktopError::InvalidBrowserHandoff)?;
        if url.username() != "" || url.password().is_some() || url.query().is_some() {
            return Err(DesktopError::InvalidBrowserHandoff);
        }
        Ok(Self { url })
    }

    pub fn url(&self) -> &Url {
        &self.url
    }

    /// Hands the safe route to the operating-system browser service.
    ///
    /// # Errors
    ///
    /// Returns an error when the platform browser service cannot be started.
    pub fn open(&self) -> Result<(), DesktopError> {
        #[cfg(target_os = "linux")]
        let status = Command::new("xdg-open").arg(self.url.as_str()).status()?;
        #[cfg(target_os = "macos")]
        let status = Command::new("open").arg(self.url.as_str()).status()?;
        #[cfg(target_os = "windows")]
        let status = Command::new("rundll32")
            .args(["url.dll,FileProtocolHandler", self.url.as_str()])
            .status()?;
        if status.success() {
            Ok(())
        } else {
            Err(DesktopError::InvalidBrowserHandoff)
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ActiveRelease {
    pub version: SchemaVersion,
    pub current: String,
    pub previous: Option<String>,
    pub digest: String,
    pub activated_at: UtcTimestamp,
}

pub struct DesktopUpdateManager {
    root: PathBuf,
}

impl DesktopUpdateManager {
    /// Opens the versioned local installation area.
    ///
    /// # Errors
    ///
    /// Returns an error for an unsafe update root.
    pub fn open(state_root: &Path) -> Result<Self, DesktopError> {
        validate_absolute_root(state_root)?;
        reject_symlink(state_root)?;
        let root = state_root.join("updates");
        fs::create_dir_all(root.join("versions"))?;
        reject_symlink(&root)?;
        Ok(Self { root })
    }

    /// Returns the canonical digest for a release directory.
    ///
    /// # Errors
    ///
    /// Returns an error for symlinks, unsafe relative paths, or unreadable content.
    pub fn digest_release(source: &Path) -> Result<String, DesktopError> {
        let mut files = Vec::new();
        collect_files(source, source, &mut files)?;
        files.sort();
        let mut hash = Sha256::new();
        for relative in files {
            hash.update(relative.to_string_lossy().as_bytes());
            hash.update([0]);
            hash.update(fs::read(source.join(&relative))?);
        }
        Ok(hex_digest(hash.finalize()))
    }

    /// Verifies, stages, re-verifies, and atomically activates a signed version directory.
    ///
    /// # Errors
    ///
    /// Returns an error for an untrusted signature, invalid version, duplicate target, or unsafe
    /// content.
    pub fn activate(
        &self,
        source: &Path,
        expected_public_key: &str,
        now: UtcTimestamp,
    ) -> Result<ActiveRelease, DesktopError> {
        let public_key = decode_public_key(expected_public_key)?;
        let verified = verify_release(source, &public_key)?;
        if verified.manifest.target
            != format!("{}-{}", std::env::consts::ARCH, std::env::consts::OS)
        {
            return Err(DesktopError::InvalidConfiguration);
        }
        verify_packaged_build_reports(source, &verified.manifest)?;
        let version = verified.manifest.version;
        if !valid_version(&version) {
            return Err(DesktopError::InvalidConfiguration);
        }
        self.ensure_trusted_public_key(&public_key)?;
        let target = self.root.join("versions").join(&version);
        if target.exists() {
            return Err(DesktopError::VersionExists);
        }
        let temporary = self
            .root
            .join("versions")
            .join(format!(".{version}-{}.tmp", EntityId::new()));
        let staged = (|| {
            copy_tree(source, &temporary)?;
            let copied = verify_release(&temporary, &public_key)?;
            if copied.manifest.version != version {
                return Err(DesktopError::InvalidConfiguration);
            }
            verify_packaged_build_reports(&temporary, &copied.manifest)?;
            let digest = Self::digest_release(&temporary)?;
            self.pin_trusted_public_key(&public_key)?;
            fs::rename(&temporary, &target)?;
            Ok(digest)
        })();
        let digest = match staged {
            Ok(digest) => digest,
            Err(error) => {
                if temporary.exists() {
                    fs::remove_dir_all(&temporary)?;
                }
                return Err(error);
            }
        };
        File::open(target.parent().ok_or(DesktopError::UnsafePath)?)?.sync_all()?;
        let previous = self.active().ok().map(|active| active.current);
        let active = ActiveRelease {
            version: CURRENT_SCHEMA_VERSION,
            current: version,
            previous,
            digest,
            activated_at: now,
        };
        atomic_json(&self.root.join("active.json"), &active)?;
        Ok(active)
    }

    /// Atomically returns to the recorded previous complete version.
    ///
    /// # Errors
    ///
    /// Returns an error when the previous release is missing, corrupt, or not recorded.
    pub fn rollback(&self, now: UtcTimestamp) -> Result<ActiveRelease, DesktopError> {
        let active = self.active()?;
        let previous = active.previous.ok_or(DesktopError::RollbackUnavailable)?;
        let target = self.root.join("versions").join(&previous);
        if !target.is_dir() {
            return Err(DesktopError::RollbackUnavailable);
        }
        let public_key = self.trusted_public_key()?;
        let verified = verify_release(&target, &public_key)?;
        if verified.manifest.version != previous {
            return Err(DesktopError::InvalidConfiguration);
        }
        verify_packaged_build_reports(&target, &verified.manifest)?;
        let digest = Self::digest_release(&target)?;
        let rolled = ActiveRelease {
            version: CURRENT_SCHEMA_VERSION,
            current: previous,
            previous: Some(active.current),
            digest,
            activated_at: now,
        };
        atomic_json(&self.root.join("active.json"), &rolled)?;
        Ok(rolled)
    }

    /// Loads the active release pointer.
    ///
    /// # Errors
    ///
    /// Returns an error for missing or malformed active state.
    pub fn active(&self) -> Result<ActiveRelease, DesktopError> {
        let active =
            serde_json::from_slice::<ActiveRelease>(&fs::read(self.root.join("active.json"))?)?;
        if active.version.major != CURRENT_SCHEMA_VERSION.major
            || !valid_version(&active.current)
            || active
                .previous
                .as_deref()
                .is_some_and(|version| !valid_version(version))
            || active.digest.len() != 64
            || !active.digest.bytes().all(|byte| byte.is_ascii_hexdigit())
        {
            return Err(DesktopError::InvalidConfiguration);
        }
        Ok(active)
    }

    /// Resolves and re-verifies the complete active version before it is executed.
    ///
    /// # Errors
    ///
    /// Returns an error when the active pointer, pinned publisher key, signed payload, target, or
    /// recorded directory digest is invalid.
    pub fn active_release_root(&self) -> Result<PathBuf, DesktopError> {
        let active = self.active()?;
        let root = self.root.join("versions").join(&active.current);
        let public_key = self.trusted_public_key()?;
        let verified = verify_release(&root, &public_key)?;
        if verified.manifest.version != active.current
            || verified.manifest.target
                != format!("{}-{}", std::env::consts::ARCH, std::env::consts::OS)
            || Self::digest_release(&root)? != active.digest
        {
            return Err(DesktopError::InvalidConfiguration);
        }
        verify_packaged_build_reports(&root, &verified.manifest)?;
        Ok(root)
    }

    fn pin_trusted_public_key(&self, public_key: &[u8; 32]) -> Result<(), DesktopError> {
        let path = self.root.join("trusted-release-key.hex");
        let encoded = keith_release::hex_encode(public_key);
        match OpenOptions::new().create_new(true).write(true).open(&path) {
            Ok(mut file) => {
                file.write_all(encoded.as_bytes())?;
                file.sync_all()?;
                File::open(&self.root)?.sync_all()?;
                Ok(())
            }
            Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => {
                if fs::read_to_string(path)?.trim() == encoded {
                    Ok(())
                } else {
                    Err(ReleaseError::UntrustedPublicKey.into())
                }
            }
            Err(error) => Err(error.into()),
        }
    }

    fn ensure_trusted_public_key(&self, public_key: &[u8; 32]) -> Result<(), DesktopError> {
        let path = self.root.join("trusted-release-key.hex");
        match fs::read_to_string(path) {
            Ok(encoded) if decode_public_key(&encoded)? == *public_key => Ok(()),
            Ok(_) => Err(ReleaseError::UntrustedPublicKey.into()),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(error) => Err(error.into()),
        }
    }

    fn trusted_public_key(&self) -> Result<[u8; 32], DesktopError> {
        decode_public_key(&fs::read_to_string(
            self.root.join("trusted-release-key.hex"),
        )?)
        .map_err(DesktopError::from)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum UninstallChoice {
    KeepUserData,
    RemoveRuntime,
    RemoveEverything,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub struct UninstallPlan {
    pub choice: UninstallChoice,
    pub exact_paths: Vec<PathBuf>,
    pub confirmation: String,
}

pub fn plan_uninstall(settings: &DesktopSettings, choice: UninstallChoice) -> UninstallPlan {
    let exact_paths = match choice {
        UninstallChoice::KeepUserData => vec![settings.state_root.join("updates")],
        UninstallChoice::RemoveRuntime => vec![
            settings.state_root.join("updates"),
            settings.state_root.join("crashes"),
            settings.state_root.join("notifications"),
            settings.data_root.join("runtime"),
            settings.daemon_socket.clone(),
        ],
        UninstallChoice::RemoveEverything => {
            vec![settings.state_root.clone(), settings.data_root.clone()]
        }
    };
    UninstallPlan {
        choice,
        exact_paths,
        confirmation: format!("REMOVE {}", settings.installation_id),
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct BackupManifest {
    version: SchemaVersion,
    created_at: UtcTimestamp,
    data_digest: String,
    notifications_digest: String,
}

/// Executes only the exact confirmed uninstall plan.
///
/// # Errors
///
/// Returns an error for mismatched confirmation, paths outside configured roots, or deletion failure.
pub fn execute_uninstall(
    settings: &DesktopSettings,
    plan: &UninstallPlan,
    confirmation: &str,
) -> Result<(), DesktopError> {
    if confirmation != plan.confirmation
        || plan.confirmation != format!("REMOVE {}", settings.installation_id)
    {
        return Err(DesktopError::ConfirmationRequired);
    }
    for path in &plan.exact_paths {
        if !(path.starts_with(&settings.state_root) || path.starts_with(&settings.data_root))
            || path == Path::new("/")
        {
            return Err(DesktopError::UnsafePath);
        }
    }
    for path in &plan.exact_paths {
        match fs::symlink_metadata(path) {
            Ok(metadata) if metadata.file_type().is_symlink() => {
                return Err(DesktopError::UnsafePath);
            }
            Ok(metadata) if metadata.is_dir() => fs::remove_dir_all(path)?,
            Ok(_) => fs::remove_file(path)?,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(error.into()),
        }
    }
    Ok(())
}

/// Creates a verified filesystem backup of desktop-owned state.
///
/// # Errors
///
/// Returns an error for unsafe source content or backup persistence failure.
pub fn backup_state(settings: &DesktopSettings) -> Result<PathBuf, DesktopError> {
    let backup_root = settings.state_root.join("backups");
    reject_symlink(&backup_root)?;
    let id = EntityId::new();
    let backup = backup_root.join(id.to_string());
    let temporary = backup_root.join(format!(".{id}.tmp"));
    fs::create_dir(&temporary)?;
    let result = (|| {
        let data = temporary.join("data");
        let notifications = temporary.join("notifications");
        copy_tree(&settings.data_root, &data)?;
        copy_tree(&settings.state_root.join("notifications"), &notifications)?;
        atomic_json(
            &temporary.join("backup.json"),
            &BackupManifest {
                version: CURRENT_SCHEMA_VERSION,
                created_at: UtcTimestamp::now().map_err(|_| DesktopError::InvalidConfiguration)?,
                data_digest: DesktopUpdateManager::digest_release(&data)?,
                notifications_digest: DesktopUpdateManager::digest_release(&notifications)?,
            },
        )?;
        fs::rename(&temporary, &backup)?;
        File::open(&backup_root)?.sync_all()?;
        Ok(())
    })();
    if let Err(error) = result {
        if temporary.exists() {
            fs::remove_dir_all(&temporary)?;
        }
        return Err(error);
    }
    Ok(backup)
}

/// Restores a verified backup into an empty target data directory.
///
/// # Errors
///
/// Returns an error for non-empty targets, symlinks, or invalid backup layout.
pub fn restore_state(backup: &Path, target_data_root: &Path) -> Result<(), DesktopError> {
    reject_symlink(backup)?;
    let manifest =
        serde_json::from_slice::<BackupManifest>(&fs::read(backup.join("backup.json"))?)?;
    if manifest.version.major != CURRENT_SCHEMA_VERSION.major
        || DesktopUpdateManager::digest_release(&backup.join("data"))? != manifest.data_digest
        || DesktopUpdateManager::digest_release(&backup.join("notifications"))?
            != manifest.notifications_digest
    {
        return Err(DesktopError::InvalidConfiguration);
    }
    if target_data_root.exists() && fs::read_dir(target_data_root)?.next().is_some() {
        return Err(DesktopError::InvalidConfiguration);
    }
    let parent = target_data_root.parent().ok_or(DesktopError::UnsafePath)?;
    fs::create_dir_all(parent)?;
    reject_symlink(parent)?;
    let name = target_data_root
        .file_name()
        .and_then(|name| name.to_str())
        .ok_or(DesktopError::UnsafePath)?;
    let temporary = parent.join(format!(".{name}-{}.tmp", EntityId::new()));
    let result = (|| {
        copy_tree(&backup.join("data"), &temporary)?;
        if DesktopUpdateManager::digest_release(&temporary)? != manifest.data_digest {
            return Err(DesktopError::InvalidConfiguration);
        }
        if target_data_root.exists() {
            reject_symlink(target_data_root)?;
            fs::remove_dir(target_data_root)?;
        }
        fs::rename(&temporary, target_data_root)?;
        File::open(parent)?.sync_all()?;
        Ok(())
    })();
    if let Err(error) = result {
        if temporary.exists() {
            fs::remove_dir_all(&temporary)?;
        }
        return Err(error);
    }
    Ok(())
}

fn validate_absolute_root(path: &Path) -> Result<(), DesktopError> {
    if !path.is_absolute()
        || path.components().any(|component| {
            matches!(
                component,
                Component::ParentDir | Component::CurDir | Component::Prefix(_)
            )
        })
        || path == Path::new("/")
    {
        Err(DesktopError::UnsafePath)
    } else {
        Ok(())
    }
}

fn validate_loopback_origin(origin: &str) -> Result<(), DesktopError> {
    let url = Url::parse(origin).map_err(|_| DesktopError::InvalidConfiguration)?;
    let valid_host = matches!(url.host_str(), Some("127.0.0.1" | "localhost" | "[::1]"));
    if url.scheme() != "http"
        || !valid_host
        || url.username() != ""
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
        || url.path() != "/"
    {
        Err(DesktopError::InvalidConfiguration)
    } else {
        Ok(())
    }
}

fn atomic_json(path: &Path, value: &impl Serialize) -> Result<(), DesktopError> {
    let parent = path.parent().ok_or(DesktopError::UnsafePath)?;
    fs::create_dir_all(parent)?;
    reject_symlink(parent)?;
    let temporary = path.with_extension(format!("{}.tmp", EntityId::new()));
    let mut file = OpenOptions::new()
        .create_new(true)
        .write(true)
        .open(&temporary)?;
    file.write_all(&keith_agent_types::canonical_json_bytes(value)?)?;
    file.sync_all()?;
    keith_platform::replace_file(&temporary, path)?;
    File::open(parent)?.sync_all()?;
    Ok(())
}

fn reject_symlink(path: &Path) -> Result<(), DesktopError> {
    if fs::symlink_metadata(path)?.file_type().is_symlink() {
        Err(DesktopError::UnsafePath)
    } else {
        Ok(())
    }
}

fn remove_stale_socket(path: &Path) -> Result<(), DesktopError> {
    #[cfg(windows)]
    {
        let _ = path;
        return Ok(());
    }
    #[cfg(unix)]
    match fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_symlink() || metadata.is_dir() => {
            Err(DesktopError::UnsafePath)
        }
        Ok(_) => {
            fs::remove_file(path)?;
            Ok(())
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error.into()),
    }
}

fn web_listener_ready(bind: &str) -> bool {
    bind.parse().ok().is_some_and(|address| {
        std::net::TcpStream::connect_timeout(&address, Duration::from_millis(50)).is_ok()
    })
}

fn poll_process(
    state_root: &Path,
    process: &mut Option<OwnedProcess>,
    kind: ManagedProcessKind,
) -> Result<Option<CrashReport>, DesktopError> {
    let Some(status) = process
        .as_mut()
        .map(|owned| owned.child.try_wait())
        .transpose()?
        .flatten()
    else {
        return Ok(None);
    };
    let owned = process.take().ok_or(DesktopError::NotOwned)?;
    let detail = read_safe_crash_detail(&owned.stderr_path)?;
    let report = CrashReport {
        version: CURRENT_SCHEMA_VERSION,
        id: EntityId::new(),
        process: kind,
        exit_code: status.code(),
        safe_detail: detail,
        observed_at: UtcTimestamp::now().map_err(|_| DesktopError::InvalidConfiguration)?,
    };
    atomic_json(
        &state_root
            .join("crashes")
            .join(format!("{}.json", report.id)),
        &report,
    )?;
    Ok(Some(report))
}

fn read_safe_crash_detail(path: &Path) -> Result<String, DesktopError> {
    let mut bytes = Vec::new();
    File::open(path)?
        .take(u64::try_from(MAX_CRASH_BYTES).unwrap_or(u64::MAX))
        .read_to_end(&mut bytes)?;
    let text = String::from_utf8_lossy(&bytes);
    let mut safe = String::new();
    for line in text.lines().take(128) {
        if contains_secret(line) {
            safe.push_str("[REDACTED]\n");
        } else {
            safe.push_str(line);
            safe.push('\n');
        }
    }
    Ok(safe)
}

fn contains_secret(value: &str) -> bool {
    let value = value.to_ascii_lowercase();
    [
        "authorization",
        "bearer ",
        "password",
        "private_key",
        "secret",
    ]
    .iter()
    .any(|marker| value.contains(marker))
}

fn stop_process(process: &mut Option<OwnedProcess>, grace: Duration) -> Result<(), DesktopError> {
    let Some(mut owned) = process.take() else {
        return Ok(());
    };
    if owned.child.try_wait()?.is_some() {
        return Ok(());
    }
    signal_terminate(&mut owned.child)?;
    let deadline = Instant::now() + grace;
    while Instant::now() < deadline {
        if owned.child.try_wait()?.is_some() {
            return Ok(());
        }
        thread::sleep(Duration::from_millis(10));
    }
    owned.child.kill()?;
    owned.child.wait()?;
    Ok(())
}

#[cfg(unix)]
fn signal_terminate(child: &mut Child) -> Result<(), DesktopError> {
    use nix::sys::signal::{Signal, kill};
    use nix::unistd::Pid;

    let pid = i32::try_from(child.id()).map_err(|_| DesktopError::InvalidConfiguration)?;
    kill(Pid::from_raw(pid), Signal::SIGTERM)
        .map_err(|error| DesktopError::Io(std::io::Error::other(error)))
}

#[cfg(not(unix))]
fn signal_terminate(child: &mut Child) -> Result<(), DesktopError> {
    child.kill().map_err(DesktopError::from)
}

fn collect_files(
    root: &Path,
    current: &Path,
    output: &mut Vec<PathBuf>,
) -> Result<(), DesktopError> {
    reject_symlink(current)?;
    for entry in fs::read_dir(current)? {
        let entry = entry?;
        let metadata = entry.file_type()?;
        if metadata.is_symlink() {
            return Err(DesktopError::UnsafePath);
        }
        if metadata.is_dir() {
            collect_files(root, &entry.path(), output)?;
        } else if metadata.is_file() {
            output.push(
                entry
                    .path()
                    .strip_prefix(root)
                    .map_err(|_| DesktopError::UnsafePath)?
                    .to_path_buf(),
            );
        } else {
            return Err(DesktopError::UnsafePath);
        }
    }
    Ok(())
}

fn copy_tree(source: &Path, target: &Path) -> Result<(), DesktopError> {
    reject_symlink(source)?;
    fs::create_dir_all(target)?;
    restrict_copied_directory(target)?;
    for entry in fs::read_dir(source)? {
        let entry = entry?;
        let file_type = entry.file_type()?;
        if file_type.is_symlink() {
            return Err(DesktopError::UnsafePath);
        }
        let destination = target.join(entry.file_name());
        if file_type.is_dir() {
            copy_tree(&entry.path(), &destination)?;
        } else if file_type.is_file() {
            let mut input = File::open(entry.path())?;
            let mut output = OpenOptions::new()
                .create_new(true)
                .write(true)
                .open(&destination)?;
            std::io::copy(&mut input, &mut output)?;
            output.sync_all()?;
            restrict_copied_file(&entry.path(), &destination)?;
        } else {
            return Err(DesktopError::UnsafePath);
        }
    }
    File::open(target)?.sync_all()?;
    Ok(())
}

#[cfg(unix)]
fn restrict_copied_directory(path: &Path) -> Result<(), DesktopError> {
    use std::os::unix::fs::PermissionsExt as _;
    fs::set_permissions(path, fs::Permissions::from_mode(0o700))?;
    Ok(())
}

#[cfg(not(unix))]
fn restrict_copied_directory(_path: &Path) -> Result<(), DesktopError> {
    Ok(())
}

#[cfg(unix)]
fn restrict_copied_file(source: &Path, target: &Path) -> Result<(), DesktopError> {
    use std::os::unix::fs::PermissionsExt as _;
    let source_mode = fs::metadata(source)?.permissions().mode();
    let target_mode = if source_mode & 0o111 == 0 {
        0o600
    } else {
        0o700
    };
    fs::set_permissions(target, fs::Permissions::from_mode(target_mode))?;
    Ok(())
}

#[cfg(not(unix))]
fn restrict_copied_file(_source: &Path, _target: &Path) -> Result<(), DesktopError> {
    Ok(())
}

fn valid_version(version: &str) -> bool {
    !version.is_empty()
        && version.len() <= 64
        && version
            .chars()
            .all(|character| character.is_ascii_alphanumeric() || matches!(character, '.' | '-'))
}

fn hex_digest(bytes: impl AsRef<[u8]>) -> String {
    bytes
        .as_ref()
        .iter()
        .fold(String::with_capacity(64), |mut output, byte| {
            write!(output, "{byte:02x}").expect("writing to a String cannot fail");
            output
        })
}

/// Generates bounded random hexadecimal material for first-run secret stores.
///
/// # Errors
///
/// Returns an error when the operating-system random source is unavailable.
pub fn random_hex(bytes: usize) -> Result<String, DesktopError> {
    let mut value = vec![0_u8; bytes];
    getrandom::fill(&mut value).map_err(|_| DesktopError::Random)?;
    Ok(hex_digest(value))
}

#[cfg(test)]
mod tests {
    use super::*;
    #[cfg(unix)]
    use std::collections::BTreeMap;

    #[cfg(unix)]
    use keith_release::{
        BuildReport, MANIFEST_FILE, MANIFEST_FORMAT, PACKAGE_NAME, PUBLIC_KEY_FILE, ReleaseFile,
        ReleaseManifest, SIGNATURE_FILE,
    };
    #[cfg(unix)]
    use ring::signature::{Ed25519KeyPair, KeyPair};

    #[cfg(unix)]
    fn signed_release(root: &Path, version: &str, payload: &[u8], seed: [u8; 32]) -> String {
        use std::os::unix::fs::PermissionsExt as _;

        fs::create_dir(root).unwrap();
        fs::create_dir(root.join("bin")).unwrap();
        for binary in ["agentd", "agent-worker"] {
            let path = root.join("bin").join(binary);
            fs::write(&path, b"#!/bin/sh\ncat \"$0.report.json\"\n").unwrap();
            fs::set_permissions(&path, fs::Permissions::from_mode(0o700)).unwrap();
        }
        fs::write(root.join("payload.bin"), payload).unwrap();
        let build_id = "desktop-release-test";
        let protocol_version = CURRENT_PROTOCOL_VERSION.to_string();
        let storage_schema = CURRENT_SCHEMA_VERSION.to_string();
        let report = |component: &str| BuildReport {
            component: component.into(),
            package_version: version.into(),
            build_id: build_id.into(),
            protocol_version: protocol_version.clone(),
            storage_schema: storage_schema.clone(),
            enabled_features: BTreeSet::from(["release-test".into()]),
        };
        let daemon_report = report("daemon");
        let worker_report = report("worker");
        fs::write(
            root.join("bin/agentd.report.json"),
            serde_json::to_vec(&daemon_report).unwrap(),
        )
        .unwrap();
        fs::write(
            root.join("bin/agent-worker.report.json"),
            serde_json::to_vec(&worker_report).unwrap(),
        )
        .unwrap();
        let mut paths = vec![
            "bin/agent-worker".to_owned(),
            "bin/agent-worker.report.json".to_owned(),
            "bin/agentd".to_owned(),
            "bin/agentd.report.json".to_owned(),
            "payload.bin".to_owned(),
        ];
        paths.sort();
        let files = paths
            .into_iter()
            .map(|path| {
                let bytes = fs::read(root.join(&path)).unwrap();
                ReleaseFile {
                    path,
                    bytes: u64::try_from(bytes.len()).unwrap(),
                    sha256: keith_release::hex_encode(&Sha256::digest(bytes)),
                }
            })
            .collect();
        let manifest = ReleaseManifest {
            format: MANIFEST_FORMAT.into(),
            package: PACKAGE_NAME.into(),
            version: version.into(),
            target: format!("{}-{}", std::env::consts::ARCH, std::env::consts::OS),
            build_id: build_id.into(),
            protocol_version,
            storage_schema,
            components: BTreeMap::from([
                ("daemon".into(), daemon_report),
                ("worker".into(), worker_report),
            ]),
            files,
        };
        let bytes = serde_json::to_vec_pretty(&manifest).unwrap();
        let key = Ed25519KeyPair::from_seed_unchecked(&seed).unwrap();
        fs::write(root.join(MANIFEST_FILE), &bytes).unwrap();
        fs::write(
            root.join(SIGNATURE_FILE),
            keith_release::hex_encode(key.sign(&bytes).as_ref()),
        )
        .unwrap();
        let public_key = keith_release::hex_encode(key.public_key().as_ref());
        fs::write(root.join(PUBLIC_KEY_FILE), &public_key).unwrap();
        public_key
    }

    #[test]
    fn first_run_notification_file_browser_backup_and_uninstall_are_scoped() {
        let directory = tempfile::tempdir().unwrap();
        let state = directory.path().join("state");
        let data = directory.path().join("data");
        let settings =
            DesktopBootstrap::initialize(&state, &data, "http://127.0.0.1:7341").unwrap();
        assert_eq!(
            DesktopBootstrap::initialize(&state, &data, "http://127.0.0.1:7341").unwrap(),
            settings
        );

        let notifications = DesktopNotificationCenter::open(&state).unwrap();
        let notification = notifications
            .notify(
                "Goal complete",
                "Verified result",
                "/goals/1",
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        assert!(
            notifications
                .acknowledge(&notification.id, UtcTimestamp::from_unix_millis(1))
                .unwrap()
                .acknowledged_at
                .is_some()
        );
        assert!(
            notifications
                .notify("unsafe", "password is abc", "/", UtcTimestamp::UNIX_EPOCH)
                .is_err()
        );

        let workspace = data.join("workspace");
        fs::create_dir(&workspace).unwrap();
        fs::write(workspace.join("result.txt"), b"result").unwrap();
        assert_eq!(
            select_file(
                &workspace.join("result.txt"),
                std::slice::from_ref(&workspace)
            )
            .unwrap()
            .bytes,
            6
        );
        assert_eq!(
            BrowserHandoff::new("http://127.0.0.1:7341", "/sessions")
                .unwrap()
                .url()
                .as_str(),
            "http://127.0.0.1:7341/sessions"
        );
        assert!(BrowserHandoff::new("https://example.com", "/sessions").is_err());

        let backup = backup_state(&settings).unwrap();
        let restored = directory.path().join("restored");
        restore_state(&backup, &restored).unwrap();
        assert_eq!(
            fs::read(restored.join("workspace/result.txt")).unwrap(),
            b"result"
        );
        fs::write(backup.join("data/workspace/result.txt"), b"tampered").unwrap();
        assert!(matches!(
            restore_state(&backup, &directory.path().join("rejected-restore")),
            Err(DesktopError::InvalidConfiguration)
        ));

        let plan = plan_uninstall(&settings, UninstallChoice::RemoveEverything);
        assert!(matches!(
            execute_uninstall(&settings, &plan, "no"),
            Err(DesktopError::ConfirmationRequired)
        ));
        execute_uninstall(&settings, &plan, &plan.confirmation).unwrap();
        assert!(!state.exists());
        assert!(!data.exists());
    }

    #[cfg(unix)]
    #[test]
    fn update_activation_digest_and_rollback_use_complete_version_directories() {
        let directory = tempfile::tempdir().unwrap();
        let state = directory.path().join("state");
        fs::create_dir(&state).unwrap();
        let manager = DesktopUpdateManager::open(&state).unwrap();
        let first = directory.path().join("release-1");
        let second = directory.path().join("release-2");
        let public_key = signed_release(&first, "1.0.0", b"version one", [7_u8; 32]);
        let second_public_key = signed_release(&second, "2.0.0", b"version two", [7_u8; 32]);
        assert_eq!(public_key, second_public_key);
        manager
            .activate(&first, &public_key, UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        assert!(matches!(
            manager.activate(
                &second,
                &keith_release::hex_encode(&[9_u8; 32]),
                UtcTimestamp::UNIX_EPOCH
            ),
            Err(DesktopError::Release(ReleaseError::UntrustedPublicKey))
        ));
        manager
            .activate(&second, &public_key, UtcTimestamp::from_unix_millis(1))
            .unwrap();
        let rolled = manager.rollback(UtcTimestamp::from_unix_millis(2)).unwrap();
        assert_eq!(rolled.current, "1.0.0");
        assert_eq!(manager.active().unwrap(), rolled);
    }
}
