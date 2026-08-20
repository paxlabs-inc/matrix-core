#![forbid(unsafe_code)]

mod events;
mod recovery;

pub use events::*;
pub use recovery::*;

use std::collections::{BTreeMap, BTreeSet};
use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Mutex, TryLockError};
use std::thread;
use std::time::{Duration, Instant};

use keith_agent_types::{
    ActionId, CURRENT_PROTOCOL_VERSION, CURRENT_SCHEMA_VERSION, CommandId, CommonError, EntityId,
    ErrorCode, Generation, ProfileId, Revision, RootTreeId, SchemaVersion, Sequence, SessionId,
    TurnId, UtcTimestamp,
};
use keith_connection::{
    AgentTransport, FramedTransport, LocalStream, accept_local, bind_permissioned_local,
    set_local_listener_nonblocking, set_local_read_timeout,
};
use keith_protocol::{
    AgentActivityKind, AgentActivityOutcome, AgentActivityProjection, ClientCommand, CommandError,
    CommandResult, CommandResultEnvelope, DaemonEvent, EventEnvelope, Feature, MessageProjection,
    ResponsePayload, SessionFilter, SessionSnapshot, SessionState, SessionSummary, ToolProjection,
    WireFormat, WireMessage, negotiate,
};
use keith_runtime_api::{
    AcceptedPrompt, RuntimeAgentOutcome, RuntimeEvent, RuntimeEventKind, RuntimeRequest,
    RuntimeResponse, RuntimeSession,
};
use keith_state_store::{EmbeddedStore, FileBackupHook, StoreError};
use keith_state_store_core::{
    AtomicStateRepository, Collection, RecordMutation, VersionedRecord, WritePrecondition,
};
use keith_supervisor::{
    SupervisorError, SupervisorOptions, WorkerEvent, WorkerStatus, WorkerSupervisor,
    signal_active_cancellation,
};
use serde::{Deserialize, Serialize};
use thiserror::Error;

pub const MAX_MANIFEST_BYTES: u64 = 64 * 1024;
const MAX_PROMPT_BYTES: usize = 256 * 1024;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "state")]
enum PromptIngressState {
    Accepted,
    Completed {
        final_id: Option<keith_agent_types::EntryId>,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct PromptIngressRecord {
    accepted: AcceptedPrompt,
    state: PromptIngressState,
    attempts: u32,
    last_error: Option<String>,
    next_attempt_at: UtcTimestamp,
}

enum PromptRunResult {
    Completed(SessionSnapshot),
    Accepted(ActionId),
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RootManifest {
    pub version: SchemaVersion,
    pub root_tree_id: RootTreeId,
    pub root_session_id: SessionId,
    pub profile_id: ProfileId,
    pub title: Option<String>,
    pub state: SessionState,
    pub updated_at: UtcTimestamp,
}

impl RootManifest {
    pub fn summary(&self) -> SessionSummary {
        SessionSummary {
            session_id: self.root_session_id.clone(),
            root_tree_id: self.root_tree_id.clone(),
            profile_id: self.profile_id.clone(),
            title: self.title.clone(),
            state: self.state,
            updated_at: self.updated_at,
        }
    }
}

#[derive(Clone, Debug, Default)]
pub struct RootCatalog {
    roots: BTreeMap<RootTreeId, RootManifest>,
    sessions: BTreeMap<SessionId, RootTreeId>,
}

#[derive(Debug, Error)]
pub enum CatalogError {
    #[error("catalog I/O failed: {0}")]
    Io(#[from] io::Error),
    #[error("manifest {path} exceeds the metadata limit of {MAX_MANIFEST_BYTES} bytes")]
    ManifestTooLarge { path: PathBuf },
    #[error("manifest {path} is invalid: {source}")]
    InvalidManifest {
        path: PathBuf,
        source: serde_json::Error,
    },
    #[error("manifest {path} uses unsupported schema {version}")]
    UnsupportedSchema {
        path: PathBuf,
        version: SchemaVersion,
    },
    #[error("manifest root {manifest} does not match directory root {directory}")]
    RootMismatch {
        manifest: RootTreeId,
        directory: RootTreeId,
    },
    #[error("duplicate root session ID {0}")]
    DuplicateSession(SessionId),
}

impl RootCatalog {
    /// Discovers root trees by reading only their bounded manifest files.
    ///
    /// Session journals, snapshots, and other root contents are deliberately never opened.
    ///
    /// # Errors
    ///
    /// Returns an error when catalog directories or manifests are malformed or unreadable.
    pub fn discover(data_root: &Path) -> Result<Self, CatalogError> {
        let sessions_directory = data_root.join("sessions");
        let entries = match fs::read_dir(&sessions_directory) {
            Ok(entries) => entries,
            Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(Self::default()),
            Err(error) => return Err(error.into()),
        };
        let mut catalog = Self::default();
        for entry in entries {
            let entry = entry?;
            if !entry.file_type()?.is_dir() {
                continue;
            }
            let directory_root: RootTreeId = match entry.file_name().to_string_lossy().parse() {
                Ok(root) => root,
                Err(_) => continue,
            };
            let path = entry.path().join("manifest.json");
            let metadata = match fs::metadata(&path) {
                Ok(metadata) => metadata,
                Err(error) if error.kind() == io::ErrorKind::NotFound => continue,
                Err(error) => return Err(error.into()),
            };
            if metadata.len() > MAX_MANIFEST_BYTES {
                return Err(CatalogError::ManifestTooLarge { path });
            }
            let bytes = fs::read(&path)?;
            let manifest: RootManifest =
                serde_json::from_slice(&bytes).map_err(|source| CatalogError::InvalidManifest {
                    path: path.clone(),
                    source,
                })?;
            if manifest.version != CURRENT_SCHEMA_VERSION {
                return Err(CatalogError::UnsupportedSchema {
                    path,
                    version: manifest.version,
                });
            }
            if manifest.root_tree_id != directory_root {
                return Err(CatalogError::RootMismatch {
                    manifest: manifest.root_tree_id,
                    directory: directory_root,
                });
            }
            if catalog
                .sessions
                .insert(
                    manifest.root_session_id.clone(),
                    manifest.root_tree_id.clone(),
                )
                .is_some()
            {
                return Err(CatalogError::DuplicateSession(manifest.root_session_id));
            }
            catalog
                .roots
                .insert(manifest.root_tree_id.clone(), manifest);
        }
        Ok(catalog)
    }

    pub fn len(&self) -> usize {
        self.roots.len()
    }

    pub fn is_empty(&self) -> bool {
        self.roots.is_empty()
    }

    pub fn root(&self, root_tree_id: &RootTreeId) -> Option<&RootManifest> {
        self.roots.get(root_tree_id)
    }

    pub fn root_for_session(&self, session_id: &SessionId) -> Option<&RootTreeId> {
        self.sessions.get(session_id)
    }

    pub fn list(&self, filter: &SessionFilter) -> Vec<SessionSummary> {
        self.roots
            .values()
            .filter(|manifest| {
                filter
                    .profile_id
                    .as_ref()
                    .is_none_or(|profile| profile == &manifest.profile_id)
            })
            .filter(|manifest| filter.include_archived || manifest.state != SessionState::Archived)
            .map(RootManifest::summary)
            .collect()
    }

    fn insert(&mut self, manifest: RootManifest) -> Result<(), CatalogError> {
        if self.sessions.contains_key(&manifest.root_session_id) {
            return Err(CatalogError::DuplicateSession(manifest.root_session_id));
        }
        self.sessions.insert(
            manifest.root_session_id.clone(),
            manifest.root_tree_id.clone(),
        );
        self.roots.insert(manifest.root_tree_id.clone(), manifest);
        Ok(())
    }
}

#[derive(Clone, Debug)]
pub struct DaemonOptions {
    pub supervisor: SupervisorOptions,
    pub idle_evict_after: Duration,
    pub maintenance_interval: Duration,
    pub runtime_maintenance_interval: Duration,
    pub replay_capacity: usize,
    pub client_queue_capacity: usize,
    pub command_dedup_capacity: usize,
}

impl Default for DaemonOptions {
    fn default() -> Self {
        Self {
            supervisor: SupervisorOptions::default(),
            idle_evict_after: Duration::from_secs(15 * 60),
            maintenance_interval: Duration::from_millis(100),
            runtime_maintenance_interval: Duration::from_secs(1),
            replay_capacity: 4_096,
            client_queue_capacity: 256,
            command_dedup_capacity: 4_096,
        }
    }
}

#[derive(Clone, Debug)]
pub struct DaemonHealth {
    pub instance_id: EntityId,
    pub discovered_roots: usize,
    pub workers: Vec<WorkerStatus>,
    pub last_worker_events: Vec<WorkerEvent>,
    pub shutting_down: bool,
}

pub struct DaemonCore {
    instance_id: EntityId,
    data_root: PathBuf,
    catalog: RootCatalog,
    supervisor: WorkerSupervisor,
    options: DaemonOptions,
    last_worker_events: Vec<WorkerEvent>,
    event_hubs: BTreeMap<RootTreeId, EventHub>,
    command_ledger: CommandLedger,
    prompt_ingress: EmbeddedStore,
    shutting_down: bool,
    startup_recovery: StartupRecoveryReport,
    worker_runtime_enabled: bool,
    last_runtime_maintenance: Option<Instant>,
}

#[derive(Debug, Error)]
pub enum DaemonError {
    #[error(transparent)]
    Catalog(#[from] CatalogError),
    #[error(transparent)]
    Supervisor(#[from] SupervisorError),
    #[error("daemon endpoint failed: {0}")]
    Connection(#[from] keith_connection::ConnectionError),
    #[error("daemon I/O failed: {0}")]
    Io(#[from] io::Error),
    #[error("daemon state lock was poisoned")]
    LockPoisoned,
    #[error("session {0} is not in the root catalog")]
    UnknownSession(SessionId),
    #[error("root {0} is not in the catalog")]
    UnknownRoot(RootTreeId),
    #[error(transparent)]
    EventStream(#[from] EventStreamError),
    #[error(transparent)]
    Recovery(#[from] RecoveryError),
    #[error("runtime operation failed: {0}")]
    Runtime(String),
    #[error(transparent)]
    State(#[from] StoreError),
    #[error("daemon prompt ingress is corrupt: {0}")]
    PromptIngress(#[from] serde_json::Error),
}

impl DaemonCore {
    /// Opens the daemon catalog and adopts live workers without activating dormant roots.
    ///
    /// # Errors
    ///
    /// Returns an error when catalog discovery or worker adoption fails.
    pub fn open(
        data_root: impl Into<PathBuf>,
        worker_executable: impl Into<PathBuf>,
        options: DaemonOptions,
    ) -> Result<Self, DaemonError> {
        Self::open_internal(data_root.into(), worker_executable.into(), options, None)
    }

    /// Opens the daemon with worker-owned session execution enabled.
    ///
    /// # Errors
    ///
    /// Returns an error when daemon state, worker configuration, or first-run bootstrap fails.
    pub fn open_with_worker_runtime(
        data_root: impl Into<PathBuf>,
        worker_executable: impl Into<PathBuf>,
        options: DaemonOptions,
        runtime_config: impl Into<PathBuf>,
    ) -> Result<Self, DaemonError> {
        let mut daemon = Self::open_internal(
            data_root.into(),
            worker_executable.into(),
            options,
            Some(runtime_config.into()),
        )?;
        if daemon.catalog.is_empty() {
            daemon.bootstrap_default_runtime_session()?;
        }
        Ok(daemon)
    }

    fn open_internal(
        data_root: PathBuf,
        worker_executable: PathBuf,
        options: DaemonOptions,
        runtime_config: Option<PathBuf>,
    ) -> Result<Self, DaemonError> {
        fs::create_dir_all(&data_root)?;
        let (catalog, startup_recovery) = recover_daemon_startup(&data_root)?;
        let worker_runtime_enabled = runtime_config.is_some();
        let mut supervisor = if let Some(runtime_config) = runtime_config {
            WorkerSupervisor::open_with_runtime_config(
                data_root.join("runtime"),
                worker_executable,
                options.supervisor.clone(),
                runtime_config,
            )?
        } else {
            WorkerSupervisor::open(
                data_root.join("runtime"),
                worker_executable,
                options.supervisor.clone(),
            )?
        };
        supervisor.adopt_existing()?;
        let command_ledger = CommandLedger::new(options.command_dedup_capacity)?;
        let prompt_ingress =
            EmbeddedStore::open(&data_root.join("state.sqlite"), Some(&FileBackupHook))?;
        Ok(Self {
            instance_id: EntityId::new(),
            data_root,
            catalog,
            supervisor,
            options,
            last_worker_events: Vec::new(),
            event_hubs: BTreeMap::new(),
            command_ledger,
            prompt_ingress,
            shutting_down: false,
            startup_recovery,
            worker_runtime_enabled,
            last_runtime_maintenance: None,
        })
    }

    pub fn catalog(&self) -> &RootCatalog {
        &self.catalog
    }

    pub fn startup_recovery(&self) -> &StartupRecoveryReport {
        &self.startup_recovery
    }

    pub fn health(&self) -> DaemonHealth {
        DaemonHealth {
            instance_id: self.instance_id.clone(),
            discovered_roots: self.catalog.len(),
            workers: self.supervisor.statuses(),
            last_worker_events: self.last_worker_events.clone(),
            shutting_down: self.shutting_down,
        }
    }

    pub fn event_hub(&self, root_tree_id: &RootTreeId) -> Option<&EventHub> {
        self.event_hubs.get(root_tree_id)
    }

    pub fn event_hub_mut(&mut self, root_tree_id: &RootTreeId) -> Option<&mut EventHub> {
        self.event_hubs.get_mut(root_tree_id)
    }

    /// Lazily activates the worker that owns a cataloged root session.
    ///
    /// # Errors
    ///
    /// Returns an error when the session is unknown or its worker cannot start.
    pub fn activate_session(
        &mut self,
        session_id: &SessionId,
    ) -> Result<WorkerStatus, DaemonError> {
        let root = self
            .catalog
            .root_for_session(session_id)
            .cloned()
            .ok_or_else(|| DaemonError::UnknownSession(session_id.clone()))?;
        let status = if self.supervisor.mark_activity(&root) {
            self.supervisor
                .status(&root)
                .ok_or_else(|| DaemonError::UnknownSession(session_id.clone()))?
        } else {
            self.supervisor.restart(&root)?
        };
        self.ensure_event_hub(&root, status.generation)?;
        Ok(status)
    }

    fn ensure_event_hub(
        &mut self,
        root_tree_id: &RootTreeId,
        generation: keith_agent_types::Generation,
    ) -> Result<(), DaemonError> {
        let manifest = self
            .catalog
            .root(root_tree_id)
            .cloned()
            .ok_or_else(|| DaemonError::UnknownRoot(root_tree_id.clone()))?;
        let worker_snapshot = if self.worker_runtime_enabled {
            match self.execute_worker(
                root_tree_id,
                generation,
                RuntimeRequest::Snapshot {
                    session_id: manifest.root_session_id.clone(),
                    generation,
                    state: manifest.state,
                },
            )? {
                RuntimeResponse::Snapshot(snapshot) => Some(*snapshot),
                response => {
                    return Err(DaemonError::Runtime(format!(
                        "worker returned {} for snapshot",
                        runtime_response_kind(&response)
                    )));
                }
            }
        } else {
            None
        };
        let snapshot = worker_snapshot.unwrap_or_else(|| SessionSnapshot {
            session: manifest.summary(),
            generation,
            through_sequence: Sequence::ZERO,
            active_action: None,
            actions: Vec::new(),
            messages: Vec::new(),
            goals: Vec::new(),
            plans: Vec::new(),
            children: Vec::new(),
            kernels: Vec::new(),
            commitments: Vec::new(),
            schedules: Vec::new(),
            tools: Vec::new(),
            confirmations: Vec::new(),
            waits: Vec::new(),
            deliveries: Vec::new(),
            memory_changes: Vec::new(),
            usage: keith_protocol::UsageProjection::default(),
            presence: keith_protocol::PresenceProjection {
                session_id: manifest.root_session_id.clone(),
                goal_id: None,
                state: keith_protocol::PresenceState::Available,
                updated_at: manifest.updated_at,
                next_wake: None,
                safe_error: None,
            },
            terminal: None,
            revision: Revision::ZERO,
        });
        match self.event_hubs.get_mut(root_tree_id) {
            Some(hub) if hub.generation() != generation => {
                hub.replace_generation(generation, snapshot)?;
            }
            Some(_) => {}
            None => {
                self.event_hubs.insert(
                    root_tree_id.clone(),
                    EventHub::new(
                        root_tree_id.clone(),
                        generation,
                        snapshot,
                        self.options.replay_capacity,
                        self.options.client_queue_capacity,
                    )?,
                );
            }
        }
        Ok(())
    }

    /// Runs worker monitoring and idle eviction once.
    ///
    /// # Errors
    ///
    /// Returns an error when worker inspection or eviction fails.
    pub fn maintain(&mut self) -> Result<(), DaemonError> {
        self.last_worker_events = self.supervisor.monitor()?;
        let failed_roots = self
            .last_worker_events
            .iter()
            .filter_map(|event| match event {
                WorkerEvent::Fatal { root_tree_id, .. } => Some(root_tree_id.clone()),
                WorkerEvent::Exited { .. } => None,
            })
            .collect::<BTreeSet<_>>();
        for root_tree_id in failed_roots {
            let _ = self.supervisor.drain(&root_tree_id);
        }
        self.supervisor.evict_idle(self.options.idle_evict_after)?;
        let runtime_maintenance_due = self.worker_runtime_enabled
            && self
                .last_runtime_maintenance
                .is_none_or(|last| last.elapsed() >= self.options.runtime_maintenance_interval);
        if runtime_maintenance_due {
            let workers = self.supervisor.statuses();
            for worker in workers {
                let healthy = matches!(
                    self.execute_worker(
                        &worker.root_tree_id,
                        worker.generation,
                        RuntimeRequest::Maintain,
                    ),
                    Ok(RuntimeResponse::Complete)
                );
                if !healthy {
                    let _ = self.supervisor.drain(&worker.root_tree_id);
                }
            }
            self.last_runtime_maintenance = Some(Instant::now());
        }
        self.resume_one_prompt();
        Ok(())
    }

    fn resume_one_prompt(&mut self) {
        let Ok(now) = UtcTimestamp::now() else {
            return;
        };
        let Ok(records) = self.prompt_ingress.list_records(Collection::PromptIngress) else {
            return;
        };
        let Some(pending) = records.into_iter().find_map(|stored| {
            serde_json::from_value::<PromptIngressRecord>(stored.payload)
                .ok()
                .filter(|record| {
                    matches!(record.state, PromptIngressState::Accepted)
                        && record.next_attempt_at <= now
                })
        }) else {
            return;
        };
        let mut discard = |_: EventEnvelope| {};
        match self.dispatch_accepted_prompt(&pending.accepted, &mut discard) {
            Ok(snapshot) => {
                let _ = self.complete_prompt(&pending.accepted.acceptance_id, &snapshot);
            }
            Err(error) => {
                let _ = self.defer_prompt(&pending.accepted.acceptance_id, &error.to_string());
            }
        }
    }

    /// Drains all active and adopted workers.
    ///
    /// # Errors
    ///
    /// Returns an error when a worker cannot be drained or forcibly terminated.
    pub fn shutdown(&mut self) -> Result<(), DaemonError> {
        self.shutting_down = true;
        self.supervisor.drain_all().map_err(DaemonError::from)
    }

    /// Serves the permission-restricted local `AgentConnection` endpoint until shutdown is signaled.
    ///
    /// # Errors
    ///
    /// Returns an error when the endpoint, protocol journey, or maintenance loop fails.
    pub fn serve_local(
        &mut self,
        socket_path: &Path,
        shutdown: &AtomicBool,
    ) -> Result<(), DaemonError> {
        if let Some(parent) = socket_path.parent() {
            fs::create_dir_all(parent)?;
        }
        match fs::remove_file(socket_path) {
            Ok(()) => {}
            Err(error) if error.kind() == io::ErrorKind::NotFound => {}
            Err(error) => return Err(error.into()),
        }
        let listener = bind_permissioned_local(socket_path)?;
        set_local_listener_nonblocking(&listener, true)?;
        let maintenance_interval = self.options.maintenance_interval;
        let instance_id = self.instance_id.clone();
        let data_root = self.data_root.clone();
        {
            let shared = Mutex::new(&mut *self);
            thread::scope(|scope| -> Result<(), DaemonError> {
                while !shutdown.load(Ordering::Acquire) {
                    match accept_local(&listener) {
                        Ok(stream) => {
                            let shared = &shared;
                            let instance_id = &instance_id;
                            let data_root = &data_root;
                            scope.spawn(move || {
                                let _ = Self::serve_shared_connection(
                                    shared,
                                    stream,
                                    shutdown,
                                    maintenance_interval,
                                    instance_id,
                                    data_root,
                                );
                            });
                        }
                        Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                            match shared.try_lock() {
                                Ok(mut daemon) => daemon.maintain()?,
                                Err(TryLockError::WouldBlock) => {}
                                Err(TryLockError::Poisoned(_)) => {
                                    return Err(DaemonError::LockPoisoned);
                                }
                            }
                            thread::sleep(maintenance_interval);
                        }
                        Err(error) => return Err(error.into()),
                    }
                }
                Ok(())
            })?;
        }
        let result = self.shutdown();
        drop(listener);
        match fs::remove_file(socket_path) {
            Err(error) if error.kind() == io::ErrorKind::NotFound => {}
            Err(error) if result.is_ok() => return Err(error.into()),
            Ok(()) | Err(_) => {}
        }
        result
    }

    #[allow(clippy::too_many_lines)]
    fn serve_shared_connection(
        shared: &Mutex<&mut Self>,
        stream: LocalStream,
        shutdown: &AtomicBool,
        maintenance_interval: Duration,
        instance_id: &EntityId,
        data_root: &Path,
    ) -> Result<(), DaemonError> {
        set_local_read_timeout(&stream, Some(maintenance_interval))?;
        let mut transport = FramedTransport::new(stream, WireFormat::Json);
        let WireMessage::ClientHello(client) = transport.receive()? else {
            return Ok(());
        };
        let connected_client_id = client.client_id.clone();
        let features = BTreeSet::from([
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
            Feature::FramedJson,
            Feature::Replay,
            Feature::Snapshots,
            Feature::DeliveryDispatch,
            Feature::AttachmentStaging,
        ]);
        let hello = negotiate(
            &client,
            CURRENT_PROTOCOL_VERSION,
            instance_id.clone(),
            &features,
        )
        .map_err(keith_connection::ConnectionError::from)?;
        let negotiated = hello.protocol;
        transport.send(&WireMessage::ServerHello(hello))?;
        while !shutdown.load(Ordering::Acquire) {
            let message = match transport.receive() {
                Ok(message) => message,
                Err(error) if error.is_timed_out() => {
                    let events = {
                        let mut daemon = shared.lock().map_err(|_| DaemonError::LockPoisoned)?;
                        daemon.maintain()?;
                        daemon.drain_client_events(&connected_client_id)?
                    };
                    for event in events {
                        transport.send(&WireMessage::from_event(event))?;
                    }
                    continue;
                }
                Err(error) if error.is_interrupted() => {
                    if shutdown.load(Ordering::Acquire) {
                        return Ok(());
                    }
                    continue;
                }
                Err(keith_connection::ConnectionError::Closed) => return Ok(()),
                Err(error) => return Err(error.into()),
            };
            let WireMessage::Command(command) = message else {
                continue;
            };
            if command.client_id == connected_client_id
                && command.protocol.major == negotiated.major
                && command.protocol.minor <= negotiated.minor
                && let ClientCommand::Cancel(keith_protocol::CancelTarget::Session(session_id)) =
                    &command.command
                && command
                    .session_id
                    .as_ref()
                    .is_none_or(|scope| scope == session_id)
                && let Ok(catalog) = RootCatalog::discover(data_root)
                && let Some(root_tree_id) = catalog.root_for_session(session_id)
            {
                let _ = signal_active_cancellation(
                    &data_root.join("runtime"),
                    root_tree_id,
                    session_id,
                    Duration::from_millis(250),
                );
            }
            let mut stream_error = None;
            let (result, recovery_events) = {
                let mut daemon = shared.lock().map_err(|_| DaemonError::LockPoisoned)?;
                daemon.handle_command_streaming(
                    &connected_client_id,
                    negotiated,
                    command,
                    &mut |event| {
                        if stream_error.is_none()
                            && let Err(error) = transport.send(&WireMessage::from_event(event))
                        {
                            stream_error = Some(error);
                        }
                    },
                )
            };
            if let Some(error) = stream_error {
                return Err(error.into());
            }
            for event in recovery_events {
                transport.send(&WireMessage::from_event(event))?;
            }
            transport.send(&WireMessage::CommandResult(result))?;
        }
        Ok(())
    }

    fn handle_command_streaming(
        &mut self,
        connected_client_id: &keith_agent_types::ClientId,
        negotiated: keith_agent_types::ProtocolVersion,
        command: keith_protocol::CommandEnvelope,
        events: &mut dyn FnMut(EventEnvelope),
    ) -> (CommandResultEnvelope, Vec<keith_protocol::EventEnvelope>) {
        let mut recovery_events = Vec::new();
        let result = if command.client_id != *connected_client_id {
            CommandResultEnvelope {
                protocol: negotiated,
                command_id: command.command_id,
                completed_at: UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH),
                result: CommandResult::Rejected(CommandError {
                    error: CommonError::new(
                        ErrorCode::Unauthorized,
                        "command client ID does not match the connection",
                        false,
                    ),
                    unsupported_feature: None,
                }),
            }
        } else if let Some(result) = self.command_ledger.result(&command.command_id) {
            result.clone()
        } else {
            let result = if command.protocol.major != negotiated.major
                || command.protocol.minor > negotiated.minor
            {
                CommandResult::Rejected(CommandError {
                    error: CommonError::new(
                        ErrorCode::UnsupportedVersion,
                        "command envelope exceeds the negotiated protocol",
                        false,
                    ),
                    unsupported_feature: None,
                })
            } else {
                let (result, events) = self.execute_command(
                    connected_client_id,
                    command.session_id.as_ref(),
                    command.command_id.clone(),
                    command.command,
                    events,
                );
                recovery_events = events;
                result
            };
            let envelope = CommandResultEnvelope {
                protocol: negotiated,
                command_id: command.command_id,
                completed_at: UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH),
                result,
            };
            self.command_ledger.record(envelope.clone());
            envelope
        };
        (result, recovery_events)
    }

    fn drain_client_events(
        &mut self,
        client_id: &keith_agent_types::ClientId,
    ) -> Result<Vec<keith_protocol::EventEnvelope>, DaemonError> {
        let mut events = Vec::new();
        let mut remaining = self.options.client_queue_capacity;
        for hub in self.event_hubs.values_mut() {
            if remaining == 0 {
                break;
            }
            match hub.poll(client_id, remaining) {
                Ok(mut pending) => {
                    remaining = remaining.saturating_sub(pending.len());
                    events.append(&mut pending);
                }
                Err(EventStreamError::UnknownClient(_)) => {}
                Err(error) => return Err(error.into()),
            }
        }
        Ok(events)
    }

    #[allow(clippy::too_many_lines)]
    fn execute_command(
        &mut self,
        client_id: &keith_agent_types::ClientId,
        scope_session_id: Option<&SessionId>,
        command_id: CommandId,
        command: ClientCommand,
        events: &mut dyn FnMut(EventEnvelope),
    ) -> (CommandResult, Vec<keith_protocol::EventEnvelope>) {
        let embedded_session_id = command_session_id(&command).cloned();
        if let (Some(scope), Some(embedded)) = (scope_session_id, embedded_session_id.as_ref())
            && scope != embedded
            && !command_supports_descendant_target(&command)
        {
            return (
                CommandResult::Rejected(CommandError {
                    error: CommonError::new(
                        ErrorCode::Unauthorized,
                        "command session does not match its connection scope",
                        false,
                    ),
                    unsupported_feature: None,
                }),
                Vec::new(),
            );
        }
        match command {
            ClientCommand::ListProfiles => {
                let result = if self.worker_runtime_enabled {
                    self.runtime_profiles()
                } else {
                    Ok(self
                        .catalog
                        .roots
                        .values()
                        .map(|manifest| keith_protocol::ProfileSummary {
                            id: manifest.profile_id.clone(),
                            workspace_id: keith_agent_types::WorkspaceId::new(),
                            display_name: manifest.profile_id.to_string(),
                            enabled: true,
                        })
                        .collect())
                };
                match result {
                    Ok(profiles) => (
                        CommandResult::Data(Box::new(ResponsePayload::Profiles(profiles))),
                        Vec::new(),
                    ),
                    Err(error) => rejected_runtime(error),
                }
            }
            ClientCommand::ListSessions(filter) => (
                CommandResult::Data(Box::new(ResponsePayload::Sessions(
                    self.catalog.list(&filter),
                ))),
                Vec::new(),
            ),
            ClientCommand::CreateSession(request) => match self.create_runtime_session(&request) {
                Ok(snapshot) => (
                    CommandResult::Data(Box::new(ResponsePayload::Snapshot(Box::new(snapshot)))),
                    Vec::new(),
                ),
                Err(error) => rejected_daemon(error),
            },
            ClientCommand::AttachSession(attach) => {
                match self.activate_and_attach(client_id, &attach) {
                    Ok(recovery) => (
                        recovery.snapshot.map_or(
                            CommandResult::Accepted { action_id: None },
                            |snapshot| {
                                CommandResult::Data(Box::new(ResponsePayload::Snapshot(Box::new(
                                    snapshot,
                                ))))
                            },
                        ),
                        recovery.events,
                    ),
                    Err(error) => (
                        CommandResult::Rejected(CommandError {
                            error: CommonError::new(ErrorCode::NotFound, error.to_string(), false),
                            unsupported_feature: None,
                        }),
                        Vec::new(),
                    ),
                }
            }
            ClientCommand::DetachSession { session_id } => {
                if let Some(root) = self.catalog.root_for_session(&session_id)
                    && let Some(hub) = self.event_hubs.get_mut(root)
                {
                    hub.detach(client_id);
                }
                (CommandResult::Accepted { action_id: None }, Vec::new())
            }
            ClientCommand::AcknowledgeEvents(acknowledgement) => {
                let result = self
                    .event_hubs
                    .get_mut(&acknowledgement.root_tree_id)
                    .ok_or_else(|| EventStreamError::UnknownClient(client_id.clone()))
                    .and_then(|hub| {
                        hub.acknowledge(
                            client_id,
                            acknowledgement.generation,
                            acknowledgement.through_sequence,
                        )
                    });
                match result {
                    Ok(()) => (CommandResult::Accepted { action_id: None }, Vec::new()),
                    Err(error) => (
                        CommandResult::Rejected(CommandError {
                            error: CommonError::new(ErrorCode::Conflict, error.to_string(), false),
                            unsupported_feature: None,
                        }),
                        Vec::new(),
                    ),
                }
            }
            ClientCommand::ResumeSession { session_id } => {
                match self.runtime_snapshot(&session_id) {
                    Ok(snapshot) => (
                        CommandResult::Data(Box::new(ResponsePayload::Snapshot(Box::new(
                            snapshot,
                        )))),
                        Vec::new(),
                    ),
                    Err(error) => rejected_daemon(error),
                }
            }
            ClientCommand::SelectModel(selection) => {
                let result = self.select_runtime_model(&selection);
                match result {
                    Ok(()) => (CommandResult::Accepted { action_id: None }, Vec::new()),
                    Err(error) => rejected_daemon(error),
                }
            }
            ClientCommand::SubmitPrompt(prompt) => match self
                .run_prompt(command_id, &prompt, events)
            {
                Ok(PromptRunResult::Completed(snapshot)) => (
                    CommandResult::Data(Box::new(ResponsePayload::Snapshot(Box::new(snapshot)))),
                    Vec::new(),
                ),
                Ok(PromptRunResult::Accepted(action_id)) => (
                    CommandResult::Accepted {
                        action_id: Some(action_id),
                    },
                    Vec::new(),
                ),
                Err(error) => rejected_daemon(error),
            },
            feature => {
                let effective_session_id = command_route_session_id(
                    scope_session_id,
                    embedded_session_id.as_ref(),
                    &feature,
                );
                let generation = match effective_session_id {
                    Some(session_id) => match self.activate_session(session_id) {
                        Ok(status) => status.generation,
                        Err(error) => return rejected_daemon(error),
                    },
                    None => Generation::ZERO,
                };
                let result = self.execute_runtime_feature(
                    client_id,
                    effective_session_id,
                    &feature,
                    generation,
                );
                match result {
                    Ok(mut result) => {
                        if !matches!(result, CommandResult::Rejected(_))
                            && let Some(session_id) = effective_session_id
                        {
                            let authoritative =
                                match self.publish_runtime_snapshot(session_id, generation) {
                                    Ok(snapshot) => snapshot,
                                    Err(error) => return rejected_daemon(error),
                                };
                            if let CommandResult::Data(payload) = &mut result
                                && matches!(payload.as_ref(), ResponsePayload::Snapshot(_))
                            {
                                **payload = ResponsePayload::Snapshot(Box::new(authoritative));
                            }
                        }
                        (result, Vec::new())
                    }
                    Err(error) => rejected_daemon(error),
                }
            }
        }
    }

    fn activate_and_attach(
        &mut self,
        client_id: &keith_agent_types::ClientId,
        attach: &keith_protocol::AttachSession,
    ) -> Result<RecoveryBatch, DaemonError> {
        self.activate_session(&attach.session_id)?;
        let root = self
            .catalog
            .root_for_session(&attach.session_id)
            .cloned()
            .ok_or_else(|| DaemonError::UnknownSession(attach.session_id.clone()))?;
        let hub = self
            .event_hubs
            .get_mut(&root)
            .ok_or(DaemonError::UnknownRoot(root))?;
        Ok(hub.attach(client_id.clone(), attach.resume.as_ref()))
    }

    fn bootstrap_default_runtime_session(&mut self) -> Result<(), DaemonError> {
        if !self.worker_runtime_enabled {
            return Err(runtime_unavailable());
        }
        let session_id = SessionId::new();
        let root_tree_id = RootTreeId::new();
        let status = self.supervisor.start(root_tree_id.clone())?;
        let response = self.execute_worker(
            &root_tree_id,
            status.generation,
            RuntimeRequest::CreateDefaultSession {
                session_id: session_id.clone(),
                root_tree_id: root_tree_id.clone(),
                title: Some("New conversation".into()),
            },
        );
        let session = match response {
            Ok(RuntimeResponse::Session(session)) => session,
            Ok(response) => {
                let _ = self.supervisor.drain(&root_tree_id);
                return Err(DaemonError::Runtime(format!(
                    "worker returned {} for default session creation",
                    runtime_response_kind(&response)
                )));
            }
            Err(error) => {
                let _ = self.supervisor.drain(&root_tree_id);
                return Err(error);
            }
        };
        self.insert_runtime_session(session, &session_id, &root_tree_id)?;
        Ok(())
    }

    fn create_runtime_session(
        &mut self,
        request: &keith_protocol::CreateSession,
    ) -> Result<SessionSnapshot, DaemonError> {
        if !self.worker_runtime_enabled {
            return Err(runtime_unavailable());
        }
        let session_id = SessionId::new();
        let root_tree_id = RootTreeId::new();
        let status = self.supervisor.start(root_tree_id.clone())?;
        let response = self.execute_worker(
            &root_tree_id,
            status.generation,
            RuntimeRequest::CreateSession {
                session_id: session_id.clone(),
                root_tree_id: root_tree_id.clone(),
                request: request.clone(),
            },
        );
        let session = match response {
            Ok(RuntimeResponse::Session(session)) => session,
            Ok(response) => {
                let _ = self.supervisor.drain(&root_tree_id);
                return Err(DaemonError::Runtime(format!(
                    "worker returned {} for session creation",
                    runtime_response_kind(&response)
                )));
            }
            Err(error) => {
                let _ = self.supervisor.drain(&root_tree_id);
                return Err(error);
            }
        };
        self.insert_runtime_session(session, &session_id, &root_tree_id)?;
        self.ensure_event_hub(&root_tree_id, status.generation)?;
        self.runtime_snapshot(&session_id)
    }

    fn insert_runtime_session(
        &mut self,
        session: RuntimeSession,
        expected_session_id: &SessionId,
        expected_root_tree_id: &RootTreeId,
    ) -> Result<(), DaemonError> {
        if session.session_id != *expected_session_id
            || session.root_tree_id != *expected_root_tree_id
        {
            return Err(DaemonError::Runtime(
                "worker created a session outside its assigned lease".into(),
            ));
        }
        let manifest = RootManifest {
            version: CURRENT_SCHEMA_VERSION,
            root_tree_id: session.root_tree_id,
            root_session_id: session.session_id,
            profile_id: session.profile_id,
            title: session.title,
            state: if session.archived {
                SessionState::Archived
            } else {
                SessionState::Dormant
            },
            updated_at: session.created_at,
        };
        self.persist_root_manifest(&manifest)?;
        self.catalog.insert(manifest)?;
        Ok(())
    }

    fn execute_worker(
        &mut self,
        root_tree_id: &RootTreeId,
        generation: Generation,
        request: RuntimeRequest,
    ) -> Result<RuntimeResponse, DaemonError> {
        if !self.worker_runtime_enabled {
            return Err(runtime_unavailable());
        }
        match self.supervisor.execute(root_tree_id, generation, request)? {
            RuntimeResponse::Failed(error) => Err(DaemonError::Runtime(error)),
            response => Ok(response),
        }
    }

    fn runtime_route(
        &mut self,
        session_id: Option<&SessionId>,
    ) -> Result<(RootTreeId, WorkerStatus), DaemonError> {
        let session_id = session_id.cloned().or_else(|| {
            self.catalog
                .roots
                .values()
                .find(|manifest| manifest.state != SessionState::Archived)
                .map(|manifest| manifest.root_session_id.clone())
        });
        let session_id = session_id.ok_or_else(runtime_unavailable)?;
        let status = self.activate_session(&session_id)?;
        let root = self
            .catalog
            .root_for_session(&session_id)
            .cloned()
            .ok_or_else(|| DaemonError::UnknownSession(session_id.clone()))?;
        Ok((root, status))
    }

    fn runtime_profiles(&mut self) -> Result<Vec<keith_protocol::ProfileSummary>, String> {
        let (root, status) = self
            .runtime_route(None)
            .map_err(|error| error.to_string())?;
        match self
            .execute_worker(&root, status.generation, RuntimeRequest::Profiles)
            .map_err(|error| error.to_string())?
        {
            RuntimeResponse::Profiles(profiles) => Ok(profiles),
            response => Err(format!(
                "worker returned {} for profile listing",
                runtime_response_kind(&response)
            )),
        }
    }

    fn select_runtime_model(
        &mut self,
        selection: &keith_protocol::ModelSelection,
    ) -> Result<(), DaemonError> {
        let (root, status) = self.runtime_route(Some(&selection.session_id))?;
        match self.execute_worker(
            &root,
            status.generation,
            RuntimeRequest::SelectModel(selection.clone()),
        )? {
            RuntimeResponse::Complete => Ok(()),
            response => Err(DaemonError::Runtime(format!(
                "worker returned {} for model selection",
                runtime_response_kind(&response)
            ))),
        }
    }

    fn execute_runtime_feature(
        &mut self,
        client_id: &keith_agent_types::ClientId,
        session_id: Option<&SessionId>,
        command: &ClientCommand,
        generation: Generation,
    ) -> Result<CommandResult, DaemonError> {
        let (root, status) = self.runtime_route(session_id)?;
        let generation = if session_id.is_some() {
            generation
        } else {
            status.generation
        };
        match self.execute_worker(
            &root,
            status.generation,
            RuntimeRequest::ExecuteFeature {
                client_id: client_id.clone(),
                scope_session_id: session_id.cloned(),
                command: command.clone(),
                generation,
            },
        )? {
            RuntimeResponse::Command(result) => Ok(*result),
            response => Err(DaemonError::Runtime(format!(
                "worker returned {} for feature command",
                runtime_response_kind(&response)
            ))),
        }
    }

    fn runtime_snapshot(&mut self, session_id: &SessionId) -> Result<SessionSnapshot, DaemonError> {
        let status = self.activate_session(session_id)?;
        let root = self
            .catalog
            .root_for_session(session_id)
            .cloned()
            .ok_or_else(|| DaemonError::UnknownSession(session_id.clone()))?;
        match self.execute_worker(
            &root,
            status.generation,
            RuntimeRequest::Snapshot {
                session_id: session_id.clone(),
                generation: status.generation,
                state: SessionState::Ready,
            },
        )? {
            RuntimeResponse::Snapshot(snapshot) => Ok(*snapshot),
            response => Err(DaemonError::Runtime(format!(
                "worker returned {} for snapshot",
                runtime_response_kind(&response)
            ))),
        }
    }

    fn publish_runtime_snapshot(
        &mut self,
        session_id: &SessionId,
        generation: Generation,
    ) -> Result<SessionSnapshot, DaemonError> {
        let root = self
            .catalog
            .root_for_session(session_id)
            .cloned()
            .ok_or_else(|| DaemonError::UnknownSession(session_id.clone()))?;
        let snapshot = match self.execute_worker(
            &root,
            generation,
            RuntimeRequest::Snapshot {
                session_id: session_id.clone(),
                generation,
                state: SessionState::Ready,
            },
        )? {
            RuntimeResponse::Snapshot(snapshot) => *snapshot,
            response => {
                return Err(DaemonError::Runtime(format!(
                    "worker returned {} for snapshot publication",
                    runtime_response_kind(&response)
                )));
            }
        };
        if let Some(hub) = self.event_hubs.get_mut(&root) {
            return hub.publish_snapshot(snapshot).map_err(Into::into);
        }
        Ok(snapshot)
    }

    fn run_prompt(
        &mut self,
        command_id: CommandId,
        prompt: &keith_protocol::SubmitPrompt,
        events: &mut dyn FnMut(EventEnvelope),
    ) -> Result<PromptRunResult, DaemonError> {
        if !self.worker_runtime_enabled {
            return Err(runtime_unavailable());
        }
        if prompt.text.trim().is_empty() || prompt.text.len() > MAX_PROMPT_BYTES {
            return Err(DaemonError::Runtime(
                "prompt is empty or exceeds the maximum accepted size".into(),
            ));
        }
        if self.catalog.root_for_session(&prompt.session_id).is_none() {
            return Err(DaemonError::UnknownSession(prompt.session_id.clone()));
        }
        let accepted = self.accept_prompt(command_id, prompt)?;
        if matches!(accepted.state, PromptIngressState::Completed { .. }) {
            return Ok(PromptRunResult::Accepted(accepted.accepted.action_id));
        }
        match self.dispatch_accepted_prompt(&accepted.accepted, events) {
            Ok(snapshot) => {
                let _ = self.complete_prompt(&accepted.accepted.acceptance_id, &snapshot);
                Ok(PromptRunResult::Completed(snapshot))
            }
            Err(error) => {
                let _ = self.defer_prompt(&accepted.accepted.acceptance_id, &error.to_string());
                Ok(PromptRunResult::Accepted(accepted.accepted.action_id))
            }
        }
    }

    fn dispatch_accepted_prompt(
        &mut self,
        accepted: &AcceptedPrompt,
        events: &mut dyn FnMut(EventEnvelope),
    ) -> Result<SessionSnapshot, DaemonError> {
        let status = self.activate_session(&accepted.prompt.session_id)?;
        let root = self
            .catalog
            .root_for_session(&accepted.prompt.session_id)
            .cloned()
            .ok_or_else(|| DaemonError::UnknownSession(accepted.prompt.session_id.clone()))?;
        let mut event_error = None;
        let response = {
            let supervisor = &mut self.supervisor;
            let mut event_hub = self.event_hubs.get_mut(&root);
            supervisor.execute_streaming(
                &root,
                status.generation,
                RuntimeRequest::RunAcceptedPrompt {
                    accepted: accepted.clone(),
                    generation: status.generation,
                },
                &mut |event| {
                    if event_error.is_some() {
                        return;
                    }
                    if let Some(hub) = event_hub.as_deref_mut() {
                        match hub.publish(runtime_daemon_event(event)) {
                            Ok(envelope) => events(envelope),
                            Err(error) => event_error = Some(error),
                        }
                    }
                },
            )?
        };
        let snapshot = match response {
            RuntimeResponse::Snapshot(snapshot) => *snapshot,
            RuntimeResponse::Failed(error) => match self
                .publish_runtime_snapshot(&accepted.prompt.session_id, status.generation)
            {
                Ok(snapshot) if snapshot.terminal.is_some() => snapshot,
                _ => return Err(DaemonError::Runtime(error)),
            },
            response => {
                return Err(DaemonError::Runtime(format!(
                    "worker returned {} for prompt",
                    runtime_response_kind(&response)
                )));
            }
        };
        let root = snapshot.session.root_tree_id.clone();
        if let Some(manifest) = self.catalog.roots.get_mut(&root) {
            manifest.state = SessionState::Ready;
            manifest.updated_at = snapshot.presence.updated_at;
            let updated = manifest.clone();
            let _ = self.persist_root_manifest(&updated);
        }
        if let Some(hub) = self.event_hubs.get_mut(&root) {
            let terminal = snapshot.terminal.clone();
            if event_error.is_none()
                && let Ok(envelope) = hub.publish(DaemonEvent::Snapshot(Box::new(snapshot.clone())))
            {
                events(envelope);
                if let Some(terminal) = terminal
                    && let Ok(envelope) = hub.publish(DaemonEvent::TurnTerminal(terminal))
                {
                    events(envelope);
                }
                return Ok(hub.snapshot().clone());
            }
        }
        Ok(snapshot)
    }

    fn accept_prompt(
        &self,
        command_id: CommandId,
        prompt: &keith_protocol::SubmitPrompt,
    ) -> Result<PromptIngressRecord, DaemonError> {
        let id = command_id.as_entity_id().clone();
        if let Some(stored) = self
            .prompt_ingress
            .get_record(Collection::PromptIngress, &id)?
        {
            let existing = serde_json::from_value::<PromptIngressRecord>(stored.payload)?;
            if existing.accepted.acceptance_id != command_id || existing.accepted.prompt != *prompt
            {
                return Err(DaemonError::Runtime(
                    "prompt command ID conflicts with a prior accepted prompt".into(),
                ));
            }
            return Ok(existing);
        }
        let accepted_at =
            UtcTimestamp::now().map_err(|error| DaemonError::Runtime(error.to_string()))?;
        let value = PromptIngressRecord {
            accepted: AcceptedPrompt {
                acceptance_id: command_id,
                action_id: ActionId::new(),
                turn_id: TurnId::new(),
                prompt: prompt.clone(),
                accepted_at,
            },
            state: PromptIngressState::Accepted,
            attempts: 0,
            last_error: None,
            next_attempt_at: accepted_at,
        };
        self.prompt_ingress.transact(&[RecordMutation::Put {
            collection: Collection::PromptIngress,
            record: VersionedRecord {
                version: CURRENT_SCHEMA_VERSION,
                id,
                revision: Revision::ZERO,
                updated_at: accepted_at,
                payload: serde_json::to_value(&value)?,
            },
            precondition: WritePrecondition::Missing,
        }])?;
        Ok(value)
    }

    fn complete_prompt(
        &self,
        acceptance_id: &CommandId,
        snapshot: &SessionSnapshot,
    ) -> Result<(), DaemonError> {
        self.update_prompt(acceptance_id, |record, now| {
            record.state = PromptIngressState::Completed {
                final_id: snapshot
                    .terminal
                    .as_ref()
                    .map(|terminal| terminal.final_id.clone()),
            };
            record.last_error = None;
            record.next_attempt_at = now;
        })
    }

    fn defer_prompt(&self, acceptance_id: &CommandId, error: &str) -> Result<(), DaemonError> {
        self.update_prompt(acceptance_id, |record, now| {
            record.attempts = record.attempts.saturating_add(1);
            record.last_error = Some(error.chars().take(1_024).collect());
            let delay_ms = i64::from(record.attempts.min(60)).saturating_mul(1_000);
            record.next_attempt_at =
                UtcTimestamp::from_unix_millis(now.unix_millis().saturating_add(delay_ms));
        })
    }

    fn update_prompt(
        &self,
        acceptance_id: &CommandId,
        update: impl FnOnce(&mut PromptIngressRecord, UtcTimestamp),
    ) -> Result<(), DaemonError> {
        let id = acceptance_id.as_entity_id();
        let stored = self
            .prompt_ingress
            .get_record(Collection::PromptIngress, id)?
            .ok_or_else(|| DaemonError::Runtime("accepted prompt record is missing".into()))?;
        let mut value = serde_json::from_value::<PromptIngressRecord>(stored.payload.clone())?;
        if matches!(value.state, PromptIngressState::Completed { .. }) {
            return Ok(());
        }
        let now = UtcTimestamp::now().map_err(|error| DaemonError::Runtime(error.to_string()))?;
        update(&mut value, now);
        let revision = stored
            .revision
            .checked_next()
            .ok_or_else(|| DaemonError::Runtime("prompt ingress revision overflow".into()))?;
        self.prompt_ingress.transact(&[RecordMutation::Put {
            collection: Collection::PromptIngress,
            record: VersionedRecord {
                version: CURRENT_SCHEMA_VERSION,
                id: id.clone(),
                revision,
                updated_at: now,
                payload: serde_json::to_value(value)?,
            },
            precondition: WritePrecondition::Exact(stored.revision),
        }])?;
        Ok(())
    }

    fn persist_root_manifest(&self, manifest: &RootManifest) -> Result<(), DaemonError> {
        let directory = self
            .data_root
            .join("sessions")
            .join(manifest.root_tree_id.to_string());
        fs::create_dir_all(&directory)?;
        let path = directory.join("manifest.json");
        let temporary = directory.join(format!(".manifest.{}.tmp", EntityId::new()));
        fs::write(
            &temporary,
            keith_agent_types::canonical_json_bytes(manifest)
                .map_err(|error| io::Error::other(error.to_string()))?,
        )?;
        fs::rename(temporary, path)?;
        Ok(())
    }
}

fn runtime_daemon_event(event: RuntimeEvent) -> DaemonEvent {
    let RuntimeEvent {
        session_id,
        turn_id,
        sequence,
        kind,
    } = event;
    match kind {
        RuntimeEventKind::AssistantDelta { message_id, text } => {
            DaemonEvent::AssistantDelta { message_id, text }
        }
        RuntimeEventKind::ToolStarted { call_id, name } => {
            DaemonEvent::ToolChanged(ToolProjection {
                tool_call_id: call_id,
                tool: Some(name),
                state: "running".into(),
                terminal: false,
            })
        }
        RuntimeEventKind::ToolCompleted {
            call_id,
            name,
            is_error,
            ..
        } => DaemonEvent::ToolChanged(ToolProjection {
            tool_call_id: call_id,
            tool: Some(name),
            state: if is_error { "failed" } else { "succeeded" }.into(),
            terminal: true,
        }),
        RuntimeEventKind::AssistantFinalCommitted {
            message_id,
            final_id,
            text,
        } => DaemonEvent::MessageCommitted(MessageProjection {
            message_id,
            final_id: Some(final_id),
            role: keith_protocol::MessageRole::Assistant,
            text,
            committed: true,
        }),
        kind => DaemonEvent::AgentActivity(AgentActivityProjection {
            session_id,
            turn_id,
            sequence,
            kind: match kind {
                RuntimeEventKind::AgentStarted => AgentActivityKind::AgentStarted,
                RuntimeEventKind::TurnStarted { number } => {
                    AgentActivityKind::TurnStarted { number }
                }
                RuntimeEventKind::AssistantStarted { message_id } => {
                    AgentActivityKind::AssistantStarted { message_id }
                }
                RuntimeEventKind::AssistantCompleted {
                    message_id,
                    complete,
                } => AgentActivityKind::AssistantCompleted {
                    message_id,
                    complete,
                },
                RuntimeEventKind::StrategyChanged { reason } => {
                    AgentActivityKind::StrategyChanged { reason }
                }
                RuntimeEventKind::TurnEnded => AgentActivityKind::TurnEnded,
                RuntimeEventKind::AgentEnded { outcome } => AgentActivityKind::AgentEnded {
                    outcome: match outcome {
                        RuntimeAgentOutcome::Completed => AgentActivityOutcome::Completed,
                        RuntimeAgentOutcome::Cancelled => AgentActivityOutcome::Cancelled,
                        RuntimeAgentOutcome::Exhausted => AgentActivityOutcome::Exhausted,
                    },
                },
                RuntimeEventKind::AssistantDelta { .. }
                | RuntimeEventKind::AssistantFinalCommitted { .. }
                | RuntimeEventKind::ToolStarted { .. }
                | RuntimeEventKind::ToolCompleted { .. } => unreachable!(),
            },
        }),
    }
}

fn runtime_unavailable() -> DaemonError {
    DaemonError::Runtime("worker runtime is disabled".into())
}

const fn runtime_response_kind(response: &RuntimeResponse) -> &'static str {
    match response {
        RuntimeResponse::Profiles(_) => "profiles",
        RuntimeResponse::Sessions(_) => "sessions",
        RuntimeResponse::Session(_) => "session",
        RuntimeResponse::Snapshot(_) => "snapshot",
        RuntimeResponse::Command(_) => "command",
        RuntimeResponse::Complete => "complete",
        RuntimeResponse::Failed(_) => "failed",
    }
}

fn command_session_id(command: &ClientCommand) -> Option<&SessionId> {
    match command {
        ClientCommand::AttachSession(request) => Some(&request.session_id),
        ClientCommand::DetachSession { session_id }
        | ClientCommand::ResumeSession { session_id }
        | ClientCommand::Cancel(keith_protocol::CancelTarget::Session(session_id))
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
        _ => None,
    }
}

const fn command_supports_descendant_target(command: &ClientCommand) -> bool {
    matches!(command, ClientCommand::CreateChild(_))
}

fn command_route_session_id<'a>(
    scope_session_id: Option<&'a SessionId>,
    embedded_session_id: Option<&'a SessionId>,
    command: &ClientCommand,
) -> Option<&'a SessionId> {
    if command_supports_descendant_target(command) {
        scope_session_id.or(embedded_session_id)
    } else {
        embedded_session_id.or(scope_session_id)
    }
}

fn rejected_runtime(error: String) -> (CommandResult, Vec<keith_protocol::EventEnvelope>) {
    rejected_daemon(DaemonError::Runtime(error))
}

#[allow(clippy::needless_pass_by_value)]
fn rejected_daemon(error: DaemonError) -> (CommandResult, Vec<keith_protocol::EventEnvelope>) {
    (
        CommandResult::Rejected(CommandError {
            error: CommonError::new(ErrorCode::Unavailable, error.to_string(), true),
            unsupported_feature: None,
        }),
        Vec::new(),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn manifest(root: RootTreeId, session: SessionId) -> RootManifest {
        RootManifest {
            version: CURRENT_SCHEMA_VERSION,
            root_tree_id: root,
            root_session_id: session,
            profile_id: ProfileId::new(),
            title: Some("catalog entry".into()),
            state: SessionState::Dormant,
            updated_at: UtcTimestamp::UNIX_EPOCH,
        }
    }

    #[test]
    fn discovery_reads_bounded_metadata_and_ignores_session_contents() {
        let directory = tempfile::tempdir().unwrap();
        let root = RootTreeId::new();
        let session = SessionId::new();
        let root_directory = directory.path().join("sessions").join(root.to_string());
        fs::create_dir_all(&root_directory).unwrap();
        fs::write(
            root_directory.join("manifest.json"),
            keith_agent_types::canonical_json_bytes(&manifest(root.clone(), session.clone()))
                .unwrap(),
        )
        .unwrap();
        fs::write(
            root_directory.join("session.jsonl"),
            b"this is deliberately corrupt and must not be loaded",
        )
        .unwrap();

        let catalog = RootCatalog::discover(directory.path()).unwrap();
        assert_eq!(catalog.len(), 1);
        assert_eq!(catalog.root_for_session(&session), Some(&root));
        assert_eq!(
            catalog.list(&SessionFilter::default())[0].session_id,
            session
        );
    }

    #[test]
    fn oversized_manifest_is_rejected_before_reading_it() {
        let directory = tempfile::tempdir().unwrap();
        let root = RootTreeId::new();
        let root_directory = directory.path().join("sessions").join(root.to_string());
        fs::create_dir_all(&root_directory).unwrap();
        fs::write(
            root_directory.join("manifest.json"),
            vec![b' '; usize::try_from(MAX_MANIFEST_BYTES + 1).unwrap()],
        )
        .unwrap();
        assert!(matches!(
            RootCatalog::discover(directory.path()),
            Err(CatalogError::ManifestTooLarge { .. })
        ));
    }

    #[test]
    fn descendant_child_creation_routes_through_the_leased_root_scope() {
        let root_session = SessionId::new();
        let child_session = SessionId::new();
        let command = ClientCommand::CreateChild(keith_protocol::CreateChild {
            parent_session_id: child_session.clone(),
            objective: "nested child".into(),
            workspace_mode: keith_protocol::ChildWorkspaceMode::ReadOnlyParent,
            limits: keith_protocol::GoalLimits {
                max_turns: Some(1),
                max_tokens: Some(1_000),
                deadline: None,
            },
        });
        assert_eq!(
            command_route_session_id(Some(&root_session), command_session_id(&command), &command,),
            Some(&root_session)
        );
        assert_eq!(command_session_id(&command), Some(&child_session));
    }
}
