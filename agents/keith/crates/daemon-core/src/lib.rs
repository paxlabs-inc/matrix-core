#![forbid(unsafe_code)]

mod events;
mod recovery;

pub use events::*;
pub use recovery::*;

use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, OpenOptions};
use std::io::{self, Read, Write};
#[cfg(unix)]
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, TryLockError};
use std::thread;
use std::time::{Duration, Instant};

use keith_agent_types::{
    ActionId, AssignmentId, CURRENT_PROTOCOL_VERSION, CURRENT_SCHEMA_VERSION, CommandId,
    CommonError, ConversationId, EntityId, ErrorCode, EventId, Generation, ProfileId, Revision,
    RootTreeId, RoundId, SchemaVersion, Sequence, SessionId, StableKey, TurnId, UtcTimestamp,
};
use keith_connection::{
    AgentTransport, FramedTransport, LocalStream, accept_local, bind_permissioned_local,
    set_local_listener_nonblocking, set_local_read_timeout,
};
use keith_protocol::{
    AgentActivityKind, AgentActivityOutcome, AgentActivityProjection, AgentLifecycleCommand,
    ClientCommand, CommandError, CommandResult, CommandResultEnvelope, ComputerProtocolCommand,
    ComputerProtocolResponse, ConversationCommand, ConversationProtocolEnvelope, DaemonEvent,
    EventEnvelope, EvolutionAvailabilityProjection, EvolutionCommand,
    EvolutionDisclosureProjection, EvolutionHypothesisProjection, EvolutionLedgerProjection,
    EvolutionProjection, Feature, MessageProjection, ResponsePayload,
    ResumeConversationEventsCommand, SessionFilter, SessionSnapshot, SessionState, SessionSummary,
    TeammatesCommand, ToolProjection, WireFormat, WireMessage, negotiate,
};
use keith_runtime_api::{
    AcceptedPrompt, ConversationSessionAssignment, RuntimeAgentOutcome, RuntimeCommandAuthority,
    RuntimeEvent, RuntimeEventKind, RuntimeRequest, RuntimeResponse, RuntimeSession,
    RuntimeWorkerBinding,
};
use keith_self_evolution::CanaryRunner;
use keith_self_evolution::{
    DaemonRestorationNotice, EvolutionEvent, EvolutionLedger, HypothesisState,
    InstallationAuthority, LedgerText, ReversalScope, ReversalTransaction, SelfEvolutionEnablement,
    StagingError, acknowledge_restoration_notice, read_pending_restoration_notice,
};
use keith_state_store::{EmbeddedStore, FileBackupHook, StoreError};
use keith_state_store_core::{
    AtomicStateRepository, Collection, RecordMutation, VersionedRecord, WritePrecondition,
};
use keith_supervisor::{
    SupervisorError, SupervisorOptions, WorkerEvent, WorkerHealth, WorkerImageRegistry,
    WorkerRollProof, WorkerStatus, WorkerSupervisor, signal_active_cancellation,
};
use keith_telemetry::{CandidateObservation, CandidateSignal, MetricName, TelemetryError};
use keith_worker_runtime::{LeaseError, LeaseManager};
use ring::rand::{SecureRandom, SystemRandom};
use serde::{Deserialize, Serialize};
use thiserror::Error;

pub const MAX_MANIFEST_BYTES: u64 = 64 * 1024;
const MAX_PROMPT_BYTES: usize = 256 * 1024;
const MAX_CANDIDATE_OBSERVATIONS: usize = 4_096;

#[derive(Clone, Debug, Error)]
#[error("{message}")]
pub struct ComputerCommandRuntimeError {
    pub code: ErrorCode,
    pub message: String,
    pub retryable: bool,
}

impl ComputerCommandRuntimeError {
    pub fn new(code: ErrorCode, message: impl Into<String>, retryable: bool) -> Self {
        Self {
            code,
            message: message.into(),
            retryable,
        }
    }
}

/// Installation-owned computer authority used by the daemon connection boundary.
pub trait ComputerCommandRuntime: Send {
    fn execute(
        &mut self,
        authenticated_client_id: &keith_agent_types::ClientId,
        daemon_instance_id: &EntityId,
        command: ComputerProtocolCommand,
    ) -> Result<ComputerProtocolResponse, ComputerCommandRuntimeError>;

    fn drain_events(
        &mut self,
        authenticated_client_id: &keith_agent_types::ClientId,
        max_events: usize,
    ) -> Result<Vec<EventEnvelope>, ComputerCommandRuntimeError>;

    fn resume_catalog(
        &mut self,
        authenticated_client_id: &keith_agent_types::ClientId,
        daemon_instance_id: &EntityId,
        request: ResumeConversationEventsCommand,
    ) -> Result<ConversationProtocolEnvelope, ComputerCommandRuntimeError>;

    fn disconnect(&mut self, authenticated_client_id: &keith_agent_types::ClientId);
}

fn read_or_create_secret(path: &Path) -> io::Result<[u8; 32]> {
    let mut options = OpenOptions::new();
    options.read(true).write(true).create_new(true);
    #[cfg(unix)]
    options.mode(0o600);
    match options.open(path) {
        Ok(mut file) => {
            let mut secret = [0_u8; 32];
            if let Err(error) = SystemRandom::new()
                .fill(&mut secret)
                .map_err(|_| io::Error::other("operating-system randomness unavailable"))
            {
                drop(file);
                let _ = fs::remove_file(path);
                return Err(error);
            }
            if let Err(error) = file.write_all(&secret).and_then(|()| file.sync_all()) {
                drop(file);
                let _ = fs::remove_file(path);
                return Err(error);
            }
            Ok(secret)
        }
        Err(error) if error.kind() == io::ErrorKind::AlreadyExists => {
            let metadata = fs::symlink_metadata(path)?;
            if metadata.file_type().is_symlink() || !metadata.file_type().is_file() {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    "daemon secret is not a regular file",
                ));
            }
            #[cfg(unix)]
            {
                use std::os::unix::fs::MetadataExt;
                if metadata.mode() & 0o077 != 0 {
                    return Err(io::Error::new(
                        io::ErrorKind::PermissionDenied,
                        "daemon secret permissions must be 0600",
                    ));
                }
            }
            let mut file = OpenOptions::new().read(true).open(path)?;
            let mut secret = [0_u8; 32];
            file.read_exact(&mut secret)?;
            let mut trailing = [0_u8; 1];
            if file.read(&mut trailing)? != 0 {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    "daemon secret has an invalid length",
                ));
            }
            Ok(secret)
        }
        Err(error) => Err(error),
    }
}

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
    #[serde(default)]
    pub session_aliases: Vec<SessionSummary>,
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

    fn summaries(&self) -> impl Iterator<Item = SessionSummary> + '_ {
        std::iter::once(self.summary()).chain(self.session_aliases.iter().cloned())
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
    #[error("session {session} is assigned to root {actual} instead of {expected}")]
    SessionRootMismatch {
        session: SessionId,
        actual: RootTreeId,
        expected: RootTreeId,
    },
    #[error("session {session} profile {actual} does not match root profile {expected}")]
    SessionProfileMismatch {
        session: SessionId,
        actual: ProfileId,
        expected: ProfileId,
    },
    #[error("session alias references unknown root {0}")]
    UnknownRoot(RootTreeId),
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
            catalog.insert(manifest)?;
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
            .flat_map(RootManifest::summaries)
            .filter(|summary| filter.include_archived || summary.state != SessionState::Archived)
            .collect()
    }

    fn insert(&mut self, manifest: RootManifest) -> Result<(), CatalogError> {
        let mut session_ids = BTreeSet::new();
        for summary in manifest.summaries() {
            if summary.root_tree_id != manifest.root_tree_id {
                return Err(CatalogError::SessionRootMismatch {
                    session: summary.session_id,
                    actual: summary.root_tree_id,
                    expected: manifest.root_tree_id.clone(),
                });
            }
            if summary.profile_id != manifest.profile_id {
                return Err(CatalogError::SessionProfileMismatch {
                    session: summary.session_id,
                    actual: summary.profile_id,
                    expected: manifest.profile_id.clone(),
                });
            }
            if !session_ids.insert(summary.session_id.clone())
                || self.sessions.contains_key(&summary.session_id)
            {
                return Err(CatalogError::DuplicateSession(summary.session_id));
            }
        }
        for session_id in session_ids {
            self.sessions
                .insert(session_id, manifest.root_tree_id.clone());
        }
        self.roots.insert(manifest.root_tree_id.clone(), manifest);
        Ok(())
    }

    fn upsert_alias(&mut self, summary: SessionSummary) -> Result<RootManifest, CatalogError> {
        let expected_root = summary.root_tree_id.clone();
        let manifest = self
            .roots
            .get_mut(&expected_root)
            .ok_or_else(|| CatalogError::UnknownRoot(expected_root.clone()))?;
        if summary.profile_id != manifest.profile_id {
            return Err(CatalogError::SessionProfileMismatch {
                session: summary.session_id,
                actual: summary.profile_id,
                expected: manifest.profile_id.clone(),
            });
        }
        if summary.session_id == manifest.root_session_id {
            manifest.profile_id = summary.profile_id;
            manifest.title = summary.title;
            manifest.state = summary.state;
            manifest.updated_at = summary.updated_at;
            return Ok(manifest.clone());
        }
        if let Some(existing_root) = self.sessions.get(&summary.session_id)
            && existing_root != &expected_root
        {
            return Err(CatalogError::DuplicateSession(summary.session_id));
        }
        self.sessions
            .insert(summary.session_id.clone(), expected_root);
        if let Some(existing) = manifest
            .session_aliases
            .iter_mut()
            .find(|existing| existing.session_id == summary.session_id)
        {
            *existing = summary;
        } else {
            manifest.session_aliases.push(summary);
        }
        Ok(manifest.clone())
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
    pub evolution_source_root: Option<PathBuf>,
}

impl Default for DaemonOptions {
    fn default() -> Self {
        Self {
            supervisor: SupervisorOptions::default(),
            idle_evict_after: Duration::from_secs(15 * 60),
            maintenance_interval: Duration::from_millis(100),
            runtime_maintenance_interval: Duration::from_secs(10),
            replay_capacity: 4_096,
            client_queue_capacity: 256,
            command_dedup_capacity: 4_096,
            evolution_source_root: None,
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

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "principal", content = "profile_id")]
pub enum TeammateCommandPrincipal {
    HumanOwner,
    Agent(ProfileId),
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "command")]
pub enum TeammateCommand {
    Refresh,
    AdvanceRead {
        conversation_id: ConversationId,
        through_sequence: u64,
    },
    ConversationAction {
        conversation_id: ConversationId,
        source_event_id: Option<EventId>,
    },
    RoundAction {
        conversation_id: ConversationId,
        round_id: RoundId,
        expected_revision: Revision,
    },
    AssignmentAction {
        conversation_id: ConversationId,
        assignment_id: AssignmentId,
        expected_revision: Revision,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AuthenticatedTeammateCommand {
    pub command_id: CommandId,
    pub operation_key: StableKey,
    pub claimed_principal: TeammateCommandPrincipal,
    pub generation: Generation,
    pub expected_through_sequence: Sequence,
    pub command: TeammateCommand,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TeammateCommandDisposition {
    Accepted,
    Replayed,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammateCommandReceipt {
    pub command_id: CommandId,
    pub operation_key: StableKey,
    pub disposition: TeammateCommandDisposition,
    pub generation: Generation,
    pub accepted_through_sequence: Sequence,
    pub action_id: Option<ActionId>,
    pub accepted_at: UtcTimestamp,
}

pub trait TeammateCommandHandler {
    type Error: std::fmt::Display;

    fn handle(
        &mut self,
        principal: &TeammateCommandPrincipal,
        command: &TeammateCommand,
    ) -> Result<Option<ActionId>, Self::Error>;
}

#[derive(Debug, Error)]
pub enum TeammateCommandRouteError {
    #[error("teammate command principal does not match authenticated authority")]
    ForgedPrincipal,
    #[error("teammate command generation or sequence fence is stale")]
    StaleFence,
    #[error("teammate operation key was replayed with different input")]
    ConflictingReplay,
    #[error("teammate command handler failed: {0}")]
    Handler(String),
    #[error("teammate command clock failed: {0}")]
    Clock(#[from] keith_agent_types::TimestampError),
}

pub struct TeammateCommandRouter<H> {
    handler: H,
    generation: Generation,
    through_sequence: Sequence,
    capacity: usize,
    order: std::collections::VecDeque<StableKey>,
    accepted: BTreeMap<StableKey, (AuthenticatedTeammateCommand, TeammateCommandReceipt)>,
}

impl<H: TeammateCommandHandler> TeammateCommandRouter<H> {
    pub fn new(
        handler: H,
        generation: Generation,
        through_sequence: Sequence,
        capacity: usize,
    ) -> Result<Self, EventStreamError> {
        if capacity == 0 {
            return Err(EventStreamError::InvalidCapacity);
        }
        Ok(Self {
            handler,
            generation,
            through_sequence,
            capacity,
            order: std::collections::VecDeque::with_capacity(capacity),
            accepted: BTreeMap::new(),
        })
    }

    pub fn update_fence(&mut self, generation: Generation, through_sequence: Sequence) {
        if generation > self.generation
            || generation == self.generation && through_sequence >= self.through_sequence
        {
            self.generation = generation;
            self.through_sequence = through_sequence;
        }
    }

    pub fn route(
        &mut self,
        authenticated_principal: &TeammateCommandPrincipal,
        command: AuthenticatedTeammateCommand,
    ) -> Result<TeammateCommandReceipt, TeammateCommandRouteError> {
        if authenticated_principal != &command.claimed_principal {
            return Err(TeammateCommandRouteError::ForgedPrincipal);
        }
        if let Some((accepted, receipt)) = self.accepted.get(&command.operation_key) {
            if accepted != &command {
                return Err(TeammateCommandRouteError::ConflictingReplay);
            }
            let mut replay = receipt.clone();
            replay.disposition = TeammateCommandDisposition::Replayed;
            return Ok(replay);
        }
        if command.generation != self.generation
            || command.expected_through_sequence != self.through_sequence
        {
            return Err(TeammateCommandRouteError::StaleFence);
        }
        let action_id = self
            .handler
            .handle(authenticated_principal, &command.command)
            .map_err(|error| TeammateCommandRouteError::Handler(error.to_string()))?;
        let receipt = TeammateCommandReceipt {
            command_id: command.command_id.clone(),
            operation_key: command.operation_key.clone(),
            disposition: TeammateCommandDisposition::Accepted,
            generation: self.generation,
            accepted_through_sequence: self.through_sequence,
            action_id,
            accepted_at: UtcTimestamp::now()?,
        };
        self.order.push_back(command.operation_key.clone());
        self.accepted
            .insert(command.operation_key.clone(), (command, receipt.clone()));
        while self.order.len() > self.capacity {
            if let Some(oldest) = self.order.pop_front() {
                self.accepted.remove(&oldest);
            }
        }
        Ok(receipt)
    }
}

pub struct DaemonCore {
    instance_id: EntityId,
    data_root: PathBuf,
    catalog: RootCatalog,
    supervisor: WorkerSupervisor,
    options: DaemonOptions,
    last_worker_events: Vec<WorkerEvent>,
    event_hubs: BTreeMap<RootTreeId, EventHub>,
    connected_clients: BTreeSet<keith_agent_types::ClientId>,
    client_session_scopes: BTreeMap<keith_agent_types::ClientId, SessionId>,
    command_ledger: CommandLedger,
    prompt_ingress: EmbeddedStore,
    shutting_down: bool,
    startup_recovery: StartupRecoveryReport,
    worker_runtime_enabled: bool,
    last_runtime_maintenance: Option<Instant>,
    pending_daemon_restoration: Option<DaemonRestorationNotice>,
    candidate_observations: Vec<CandidateObservation>,
    evolution: EvolutionController,
    computer_runtime: Option<Box<dyn ComputerCommandRuntime>>,
}

struct ClientScopeCleanup<'shared, 'daemon> {
    shared: &'shared Mutex<&'daemon mut DaemonCore>,
    client_id: keith_agent_types::ClientId,
}

impl Drop for ClientScopeCleanup<'_, '_> {
    fn drop(&mut self) {
        if let Ok(mut daemon) = self.shared.lock() {
            daemon.connected_clients.remove(&self.client_id);
            daemon.client_session_scopes.remove(&self.client_id);
            if let Some(runtime) = daemon.computer_runtime.as_mut() {
                runtime.disconnect(&self.client_id);
            }
        }
    }
}

struct EvolutionController {
    source_root: PathBuf,
    ledger: Arc<EvolutionLedger<EmbeddedStore>>,
    enablement: SelfEvolutionEnablement<EmbeddedStore>,
    authority: InstallationAuthority,
}

#[derive(Debug, Error)]
pub enum DaemonError {
    #[error(transparent)]
    Catalog(#[from] CatalogError),
    #[error(transparent)]
    Supervisor(#[from] SupervisorError),
    #[error("canary recovery failed: {0}")]
    Canary(#[from] keith_self_evolution::CanaryError),
    #[error("daemon replacement recovery failed: {0}")]
    Staging(#[from] StagingError),
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
    #[error(transparent)]
    Telemetry(#[from] TelemetryError),
    #[error(transparent)]
    WorkerLease(#[from] LeaseError),
    #[error("self-evolution controller failed: {0}")]
    Evolution(String),
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
        CanaryRunner::open(
            data_root.join("self-evolution").join("canaries"),
            options.supervisor.clone(),
        )?;
        let (catalog, startup_recovery) = recover_daemon_startup(&data_root)?;
        let pending_daemon_restoration = read_pending_restoration_notice(
            data_root.join("self-evolution").join("daemon-images"),
        )?;
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
        let evolution_store = Arc::new(EmbeddedStore::open(
            &data_root.join("state.sqlite"),
            Some(&FileBackupHook),
        )?);
        let evolution_root = data_root.join("self-evolution");
        fs::create_dir_all(&evolution_root)?;
        let signing_seed = read_or_create_secret(&evolution_root.join("ledger.seed"))?;
        let authority_secret = read_or_create_secret(&evolution_root.join("authority.seed"))?;
        let ledger = Arc::new(
            EvolutionLedger::from_seed(evolution_store, &signing_seed)
                .map_err(|error| DaemonError::Evolution(error.to_string()))?,
        );
        let source_root = options
            .evolution_source_root
            .clone()
            .unwrap_or_else(|| data_root.clone());
        let enabled = ledger
            .records()
            .map_err(|error| DaemonError::Evolution(error.to_string()))?
            .iter()
            .rev()
            .find_map(|record| match record.event {
                EvolutionEvent::Enable { .. } => Some(true),
                EvolutionEvent::Disable { .. } => Some(false),
                _ => None,
            })
            .unwrap_or(false);
        let enablement = SelfEvolutionEnablement::new_restored(
            source_root.clone(),
            authority_secret,
            "installation-owner".into(),
            Arc::clone(&ledger),
            enabled,
        );
        let authority = enablement
            .authenticate_installation(&authority_secret)
            .map_err(|error| DaemonError::Evolution(error.to_string()))?;
        let evolution = EvolutionController {
            source_root,
            ledger,
            enablement,
            authority,
        };
        Ok(Self {
            instance_id: EntityId::new(),
            data_root,
            catalog,
            supervisor,
            options,
            last_worker_events: Vec::new(),
            event_hubs: BTreeMap::new(),
            connected_clients: BTreeSet::new(),
            client_session_scopes: BTreeMap::new(),
            command_ledger,
            prompt_ingress,
            shutting_down: false,
            startup_recovery,
            worker_runtime_enabled,
            last_runtime_maintenance: Some(Instant::now()),
            pending_daemon_restoration,
            candidate_observations: Vec::new(),
            evolution,
            computer_runtime: None,
        })
    }

    /// Installs the installation-owned live-computer command and event authority.
    pub fn install_computer_runtime(&mut self, runtime: Box<dyn ComputerCommandRuntime>) {
        if let Some(mut previous) = self.computer_runtime.replace(runtime) {
            for client_id in &self.connected_clients {
                previous.disconnect(client_id);
            }
        }
    }

    pub fn computer_streaming_available(&self) -> bool {
        self.computer_runtime.is_some()
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

    /// Removes and returns candidate observations accumulated by daemon maintenance.
    pub fn take_candidate_observations(&mut self) -> Vec<CandidateObservation> {
        std::mem::take(&mut self.candidate_observations)
    }

    /// Records a turn-level candidate signal only when its image and generation still identify
    /// the active worker. This closes attribution before watchdog consumption.
    ///
    /// # Errors
    ///
    /// Returns an error when the root is inactive or the supplied candidate identity is stale.
    pub fn record_candidate_signal(
        &mut self,
        root_tree_id: &RootTreeId,
        generation: Generation,
        image_id: &str,
        signal: CandidateSignal,
    ) -> Result<(), DaemonError> {
        let worker = self
            .supervisor
            .status(root_tree_id)
            .filter(|worker| worker.generation == generation && worker.image_id == image_id)
            .ok_or_else(|| {
                DaemonError::Runtime("candidate observation attribution is stale".into())
            })?;
        self.push_candidate_observation(CandidateObservation::new(
            worker.image_id,
            worker.root_tree_id,
            worker.generation,
            UtcTimestamp::now().map_err(|error| DaemonError::Runtime(error.to_string()))?,
            signal,
        )?);
        Ok(())
    }

    /// Records a hypothesis metric against an active candidate worker.
    ///
    /// # Errors
    ///
    /// Returns an error when the candidate attribution is stale or the clock is unavailable.
    pub fn record_candidate_metric(
        &mut self,
        root_tree_id: &RootTreeId,
        generation: Generation,
        image_id: &str,
        metric: MetricName,
        value: u64,
    ) -> Result<(), DaemonError> {
        self.record_candidate_signal(
            root_tree_id,
            generation,
            image_id,
            CandidateSignal::HypothesisMetric { metric, value },
        )
    }

    fn push_candidate_observation(&mut self, observation: CandidateObservation) {
        if self.candidate_observations.len() == MAX_CANDIDATE_OBSERVATIONS {
            self.candidate_observations.remove(0);
        }
        self.candidate_observations.push(observation);
    }

    #[must_use]
    pub const fn worker_image_registry(&self) -> &WorkerImageRegistry {
        self.supervisor.image_registry()
    }

    pub const fn worker_image_registry_mut(&mut self) -> &mut WorkerImageRegistry {
        self.supervisor.image_registry_mut()
    }

    pub fn event_hub(&self, root_tree_id: &RootTreeId) -> Option<&EventHub> {
        self.event_hubs.get(root_tree_id)
    }

    pub fn event_hub_mut(&mut self, root_tree_id: &RootTreeId) -> Option<&mut EventHub> {
        self.event_hubs.get_mut(root_tree_id)
    }

    /// Returns the cataloged roots that currently have an active worker generation.
    pub fn active_worker_roots(&self) -> Vec<RootTreeId> {
        self.supervisor.active_roots()
    }

    /// Rolls one active root to one exact installed image and refreshes its event hub in place.
    ///
    /// The daemon process and unrelated roots remain alive. The supervisor restores the exact
    /// previous image if candidate startup fails.
    ///
    /// # Errors
    ///
    /// Returns an error when the root is unknown, rolling fails, or the replacement generation's
    /// snapshot cannot seed the event hub.
    pub fn roll_worker_to_image(
        &mut self,
        root_tree_id: &RootTreeId,
        image_id: &str,
    ) -> Result<WorkerRollProof, DaemonError> {
        if self.catalog.root(root_tree_id).is_none() {
            return Err(DaemonError::UnknownRoot(root_tree_id.clone()));
        }
        let proof = self.supervisor.roll_to_image(root_tree_id, image_id)?;
        self.refresh_event_hub(root_tree_id, proof.generation)?;
        Ok(proof)
    }

    /// Rebuilds or replaces one root's event hub for an already active generation without
    /// restarting the daemon.
    pub fn refresh_event_hub(
        &mut self,
        root_tree_id: &RootTreeId,
        generation: Generation,
    ) -> Result<(), DaemonError> {
        self.ensure_event_hub(root_tree_id, generation)
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
        let status = self.supervisor.status(&root);
        let status = if let Some(status) = status
            && status.health != WorkerHealth::Exited
            && self
                .supervisor
                .validate_route(&root, status.generation)
                .is_ok()
            && self.supervisor.mark_activity(&root)
        {
            status
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
        if self
            .event_hubs
            .get(root_tree_id)
            .is_some_and(|hub| hub.generation() == generation)
        {
            return Ok(());
        }
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
        let workers_before_monitor = self
            .supervisor
            .statuses()
            .into_iter()
            .map(|worker| ((worker.root_tree_id.clone(), worker.generation), worker))
            .collect::<BTreeMap<_, _>>();
        self.last_worker_events = self.supervisor.monitor()?;
        let observed_at =
            UtcTimestamp::now().map_err(|error| DaemonError::Runtime(error.to_string()))?;
        let mut observations = Vec::new();
        for event in &self.last_worker_events {
            let (root_tree_id, generation, crashed) = match event {
                WorkerEvent::Fatal {
                    root_tree_id,
                    generation,
                    ..
                } => (root_tree_id, *generation, true),
                WorkerEvent::Exited {
                    root_tree_id,
                    generation,
                    success,
                } => (root_tree_id, *generation, *success != Some(true)),
            };
            if crashed
                && let Some(worker) =
                    workers_before_monitor.get(&(root_tree_id.clone(), generation))
            {
                observations.push(CandidateObservation::new(
                    worker.image_id.clone(),
                    root_tree_id.clone(),
                    generation,
                    observed_at,
                    CandidateSignal::WorkerCrash,
                )?);
            }
        }
        for worker in self.supervisor.statuses() {
            observations.push(CandidateObservation::new(
                worker.image_id,
                worker.root_tree_id,
                worker.generation,
                observed_at,
                CandidateSignal::ResourceUse {
                    resident_bytes: worker.resources.resident_bytes,
                    virtual_bytes: worker.resources.virtual_bytes,
                },
            )?);
        }
        for observation in observations {
            self.push_candidate_observation(observation);
        }
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
                // A maintenance-domain failure is not evidence that the authenticated worker
                // process died. Runtime failures arrive as `RuntimeResponse::Failed`, while a
                // supervisor error can also be a transient control-channel condition. Monitoring
                // above is the authoritative process-health boundary and drains confirmed fatal
                // workers; maintenance must never turn its own probe failure into cancellation of
                // an otherwise live provider/tool turn.
                let _ = self.supervisor.execute(
                    &worker.root_tree_id,
                    worker.generation,
                    RuntimeRequest::Maintain,
                );
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
        self.serve_local_with_ready(socket_path, shutdown, || Ok(()))
    }

    /// Serves the local endpoint and reports positive readiness only after recovery and binding.
    ///
    /// # Errors
    /// Returns an error when endpoint setup, the readiness acknowledgement, or serving fails.
    pub fn serve_local_with_ready(
        &mut self,
        socket_path: &Path,
        shutdown: &AtomicBool,
        ready: impl FnOnce() -> Result<(), DaemonError>,
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
        self.startup_recovery.mark_endpoints_ready();
        if startup_fault_requested("readiness") {
            drop(listener);
            let _ = fs::remove_file(socket_path);
            return Err(DaemonError::Runtime(
                "injected daemon readiness failure".into(),
            ));
        }
        if let Err(error) = ready() {
            drop(listener);
            let _ = fs::remove_file(socket_path);
            return Err(error);
        }
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
        shared
            .lock()
            .map_err(|_| DaemonError::LockPoisoned)?
            .connected_clients
            .insert(connected_client_id.clone());
        let _scope_cleanup = ClientScopeCleanup {
            shared,
            client_id: connected_client_id.clone(),
        };
        let mut features = BTreeSet::from([
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
            Feature::SelfEvolution,
            Feature::AgentLifecycle,
            Feature::Conversations,
        ]);
        if shared
            .lock()
            .map_err(|_| DaemonError::LockPoisoned)?
            .computer_streaming_available()
        {
            features.insert(Feature::ComputerStreaming);
        }
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
                let requester_scope = effective_requester_scope(
                    self.client_session_scopes.get(connected_client_id),
                    command.session_id.as_ref(),
                )
                .cloned();
                let (result, events) = self.execute_command(
                    connected_client_id,
                    requester_scope.as_ref(),
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
        if remaining > 0
            && let Some(runtime) = self.computer_runtime.as_mut()
        {
            let mut pending = runtime
                .drain_events(client_id, remaining)
                .map_err(|error| DaemonError::Runtime(error.to_string()))?;
            if pending.len() > remaining
                || pending.iter().any(|event| {
                    !matches!(
                        event.event,
                        DaemonEvent::Computer(_) | DaemonEvent::Teammates(_)
                    )
                })
            {
                return Err(DaemonError::Runtime(
                    "computer runtime returned an invalid client event batch".into(),
                ));
            }
            events.append(&mut pending);
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
        if command_rejects_session_scope(&command) && scope_session_id.is_some() {
            return (
                CommandResult::Rejected(CommandError {
                    error: CommonError::new(
                        ErrorCode::Unauthorized,
                        "installation administration cannot inherit a session scope",
                        false,
                    ),
                    unsupported_feature: None,
                }),
                Vec::new(),
            );
        }
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
            ClientCommand::Computer(command) => {
                let Some(runtime) = self.computer_runtime.as_mut() else {
                    return (
                        CommandResult::Rejected(CommandError {
                            error: CommonError::new(
                                ErrorCode::Unavailable,
                                "computer streaming is unavailable",
                                true,
                            ),
                            unsupported_feature: Some(Feature::ComputerStreaming),
                        }),
                        Vec::new(),
                    );
                };
                match runtime.execute(client_id, &self.instance_id, command) {
                    Ok(response) => (
                        CommandResult::Data(Box::new(ResponsePayload::Computer(Box::new(
                            response,
                        )))),
                        Vec::new(),
                    ),
                    Err(error) => (
                        CommandResult::Rejected(CommandError {
                            error: CommonError::new(error.code, error.message, error.retryable),
                            unsupported_feature: None,
                        }),
                        Vec::new(),
                    ),
                }
            }
            ClientCommand::Conversation(ConversationCommand::Teammates(
                TeammatesCommand::Resume(request),
            )) if request.conversation_id.is_none() && request.profile_id.is_some() => {
                let Some(runtime) = self.computer_runtime.as_mut() else {
                    return (
                        CommandResult::Rejected(CommandError {
                            error: CommonError::new(
                                ErrorCode::Unavailable,
                                "computer catalog streaming is unavailable",
                                true,
                            ),
                            unsupported_feature: Some(Feature::ComputerStreaming),
                        }),
                        Vec::new(),
                    );
                };
                match runtime.resume_catalog(client_id, &self.instance_id, request) {
                    Ok(envelope) => (
                        CommandResult::Data(Box::new(ResponsePayload::TeammatesEvent(Box::new(
                            envelope,
                        )))),
                        Vec::new(),
                    ),
                    Err(error) => (
                        CommandResult::Rejected(CommandError {
                            error: CommonError::new(error.code, error.message, error.retryable),
                            unsupported_feature: None,
                        }),
                        Vec::new(),
                    ),
                }
            }
            ClientCommand::Evolution(command) => self.execute_evolution_command(command),
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
                            background: None,
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
                    Ok(recovery) => {
                        self.client_session_scopes
                            .insert(client_id.clone(), attach.session_id.clone());
                        (
                            recovery.snapshot.map_or(
                                CommandResult::Accepted { action_id: None },
                                |snapshot| {
                                    CommandResult::Data(Box::new(ResponsePayload::Snapshot(
                                        Box::new(snapshot),
                                    )))
                                },
                            ),
                            recovery.events,
                        )
                    }
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
                // Detaching stops event delivery but deliberately does not erase requester
                // origin. A session-bound client cannot turn itself into the installation owner
                // by detaching and sending a scope-free command on the same authenticated client.
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
                let requester_authority =
                    match runtime_command_authority(&self.catalog, scope_session_id) {
                        Ok(authority) => authority,
                        Err(error) => return rejected_daemon(error),
                    };
                if let ClientCommand::Conversation(ConversationCommand::Teammates(
                    TeammatesCommand::PeerMessage(request),
                )) = &feature
                    && peer_message_provisioning_authorized(
                        &self.catalog,
                        &requester_authority,
                        scope_session_id,
                        request,
                    )
                    && let Err(error) = self.provision_conversation_participant_session(
                        &request.conversation_id,
                        &request.recipient_profile_id,
                    )
                {
                    return rejected_daemon(error);
                }
                let group_provisioning = match &feature {
                    ClientCommand::Conversation(ConversationCommand::Teammates(
                        TeammatesCommand::CreateGroup(request),
                    )) => Some(request.initial_profile_ids.clone()),
                    _ => None,
                };
                let enabled_profile = match &feature {
                    ClientCommand::AgentLifecycle(AgentLifecycleCommand::Enable(request)) => {
                        Some(request.profile_id.clone())
                    }
                    _ => None,
                };
                let conversation_wake = match &feature {
                    ClientCommand::Conversation(ConversationCommand::Teammates(
                        TeammatesCommand::PeerMessage(request),
                    )) => Some(request.conversation_id.clone()),
                    ClientCommand::Conversation(ConversationCommand::Teammates(
                        TeammatesCommand::StartRound(request),
                    )) => Some(request.conversation_id.clone()),
                    _ => None,
                };
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
                    requester_authority,
                    effective_session_id,
                    &feature,
                    generation,
                );
                match result {
                    Ok(mut result) => {
                        let branched_session = match (&feature, &result) {
                            (
                                ClientCommand::BranchSession(request),
                                CommandResult::Data(payload),
                            ) => match payload.as_ref() {
                                ResponsePayload::Snapshot(snapshot)
                                    if snapshot.session.session_id != request.session_id =>
                                {
                                    Some(snapshot.session.clone())
                                }
                                _ => None,
                            },
                            _ => None,
                        };
                        if let Some(summary) = &branched_session
                            && let Err(error) = self.register_runtime_session_alias(summary)
                        {
                            return rejected_daemon(error);
                        }
                        if !matches!(result, CommandResult::Rejected(_))
                            && let Some(profile_id) = enabled_profile
                            && let Err(error) = self.profile_runtime_root(&profile_id)
                        {
                            return rejected_daemon(error);
                        }
                        if !matches!(result, CommandResult::Rejected(_))
                            && let Some(profile_ids) = group_provisioning
                        {
                            let Some(conversation_id) = command_result_conversation_id(&result)
                            else {
                                return rejected_daemon(DaemonError::Runtime(
                                    "group creation returned no canonical conversation identity"
                                        .into(),
                                ));
                            };
                            if let Err(error) = self.provision_group_participant_sessions(
                                effective_session_id,
                                &conversation_id,
                                &profile_ids,
                            ) {
                                return rejected_daemon(error);
                            }
                        }
                        if !matches!(result, CommandResult::Rejected(_))
                            && let Some(conversation_id) = conversation_wake
                            && let Err(error) = self.wake_conversation_actions(&conversation_id)
                        {
                            return rejected_daemon(error);
                        }
                        if branched_session.is_none()
                            && !matches!(result, CommandResult::Rejected(_))
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
        let is_alias = self
            .catalog
            .root(&root)
            .is_some_and(|manifest| manifest.root_session_id != attach.session_id);
        let alias_snapshot = if is_alias {
            Some(self.runtime_snapshot(&attach.session_id)?)
        } else {
            None
        };
        let pending = self.pending_daemon_restoration.clone();
        let mut recovery = {
            let hub = self
                .event_hubs
                .get_mut(&root)
                .ok_or(DaemonError::UnknownRoot(root))?;
            let recovery = hub.attach(client_id.clone(), attach.resume.as_ref());
            if let Some(notice) = &pending {
                hub.publish(DaemonEvent::Warning(CommonError::new(
                    ErrorCode::Unavailable,
                    format!(
                        "Keith restored the previous daemon after a staged update failed to start: {}. The failed image remains available for inspection.",
                        notice.reason
                    ),
                    false,
                )))?;
            }
            recovery
        };
        if let Some(snapshot) = alias_snapshot {
            recovery.mode = keith_protocol::ResumeMode::SnapshotThenDelta;
            recovery.snapshot = Some(snapshot);
            recovery.events.clear();
        }
        if let Some(notice) = pending {
            acknowledge_restoration_notice(
                self.data_root.join("self-evolution").join("daemon-images"),
                &notice.notice_id,
            )?;
            self.pending_daemon_restoration = None;
        }
        Ok(recovery)
    }

    fn execute_evolution_command(
        &mut self,
        command: EvolutionCommand,
    ) -> (CommandResult, Vec<keith_protocol::EventEnvelope>) {
        if matches!(command, EvolutionCommand::Enable { .. }) {
            return rejected_evolution(
                ErrorCode::Unauthorized,
                "Clients cannot enable self-evolution. Use the installation-owner control on the daemon host after reviewing the complete disclosure.",
            );
        }
        let now = match UtcTimestamp::now() {
            Ok(now) => now,
            Err(error) => return rejected_evolution(ErrorCode::Internal, &error.to_string()),
        };
        let page = match &command {
            EvolutionCommand::BrowseLedger {
                before_sequence,
                limit,
            } => (*before_sequence, usize::from((*limit).clamp(1, 200))),
            _ => (None, 100),
        };
        let mutation = match command {
            EvolutionCommand::Status | EvolutionCommand::BrowseLedger { .. } => Ok(()),
            EvolutionCommand::Disable { reason } => self
                .evolution
                .enablement
                .disable(&self.evolution.authority, &reason, now)
                .map_err(|error| error.to_string()),
            EvolutionCommand::Approve { hypothesis_id } => {
                record_evolution_approval(&self.evolution, hypothesis_id, now)
            }
            EvolutionCommand::Revert {
                promotion_id,
                reason,
            } => reverse_evolution(
                &mut self.supervisor,
                &self.data_root,
                &self.evolution,
                ReversalScope::Promotion(promotion_id),
                &reason,
                now,
            ),
            EvolutionCommand::RestoreBaseline { reason } => reverse_evolution(
                &mut self.supervisor,
                &self.data_root,
                &self.evolution,
                ReversalScope::HumanBaseline,
                &reason,
                now,
            ),
            EvolutionCommand::Enable { .. } => unreachable!(),
        };
        if let Err(error) = mutation {
            return rejected_evolution(ErrorCode::Conflict, &error);
        }
        match evolution_projection(&self.evolution, page.0, page.1) {
            Ok(projection) => {
                for hub in self.event_hubs.values_mut() {
                    let _ =
                        hub.publish(DaemonEvent::EvolutionChanged(Box::new(projection.clone())));
                }
                (
                    CommandResult::Data(Box::new(ResponsePayload::Evolution(Box::new(projection)))),
                    Vec::new(),
                )
            }
            Err(error) => rejected_evolution(ErrorCode::Internal, &error),
        }
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
        self.ensure_event_hub(&root_tree_id, status.generation)?;
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
            session_aliases: Vec::new(),
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

    fn register_runtime_session_alias(
        &mut self,
        summary: &SessionSummary,
    ) -> Result<(), DaemonError> {
        let manifest = self.catalog.upsert_alias(summary.clone())?;
        self.persist_root_manifest(&manifest)
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

    fn wake_conversation_actions(
        &mut self,
        conversation_id: &ConversationId,
    ) -> Result<(), DaemonError> {
        let (query_root, query_status) = self.runtime_route(None)?;
        let response = self.execute_worker(
            &query_root,
            query_status.generation,
            RuntimeRequest::PendingConversationActionSessions {
                conversation_id: conversation_id.clone(),
            },
        )?;
        let RuntimeResponse::Sessions(sessions) = response else {
            return Err(DaemonError::Runtime(format!(
                "worker returned {} for pending conversation action routing",
                runtime_response_kind(&response)
            )));
        };
        let mut active_roots = BTreeMap::new();
        for session in sessions {
            let catalog_root = self
                .catalog
                .root_for_session(&session.session_id)
                .ok_or_else(|| DaemonError::UnknownSession(session.session_id.clone()))?;
            if catalog_root != &session.root_tree_id || session.archived {
                return Err(DaemonError::Runtime(
                    "pending conversation action resolved outside its cataloged active root".into(),
                ));
            }
            let status = self.activate_session(&session.session_id)?;
            active_roots.insert(session.root_tree_id, status.generation);
        }
        for (root_tree_id, generation) in active_roots {
            self.execute_worker(
                &root_tree_id,
                generation,
                RuntimeRequest::DrainConversationActions {
                    conversation_id: conversation_id.clone(),
                    generation,
                },
            )?;
        }
        Ok(())
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

    fn profile_runtime_root(
        &mut self,
        profile_id: &ProfileId,
    ) -> Result<(RootTreeId, WorkerStatus), DaemonError> {
        let existing = self
            .catalog
            .roots
            .values()
            .filter(|manifest| {
                manifest.profile_id == *profile_id && manifest.state != SessionState::Archived
            })
            .max_by_key(|manifest| manifest.updated_at)
            .cloned();
        if let Some(manifest) = existing {
            let status = self.activate_session(&manifest.root_session_id)?;
            return Ok((manifest.root_tree_id, status));
        }
        let profile = self
            .runtime_profiles()
            .map_err(DaemonError::Runtime)?
            .into_iter()
            .find(|profile| &profile.id == profile_id && profile.enabled)
            .ok_or_else(|| {
                DaemonError::Runtime(format!(
                    "conversation participant profile {profile_id} has no enabled runtime"
                ))
            })?;
        let snapshot = self.create_runtime_session(&keith_protocol::CreateSession {
            profile_id: profile.id,
            workspace_id: profile.workspace_id,
            title: Some("New conversation".into()),
        })?;
        let root_tree_id = snapshot.session.root_tree_id.clone();
        let status = self.activate_session(&snapshot.session.session_id)?;
        Ok((root_tree_id, status))
    }

    fn provision_conversation_participant_session(
        &mut self,
        conversation_id: &ConversationId,
        profile_id: &ProfileId,
    ) -> Result<SessionId, DaemonError> {
        let (root_tree_id, status) = self.profile_runtime_root(profile_id)?;
        let response = self.execute_worker(
            &root_tree_id,
            status.generation,
            RuntimeRequest::ProvisionConversationSession {
                conversation_id: conversation_id.clone(),
                profile_id: profile_id.clone(),
                generation: status.generation,
                now: UtcTimestamp::now().map_err(|error| {
                    DaemonError::Runtime(format!(
                        "conversation provisioning clock is unavailable: {error}"
                    ))
                })?,
            },
        )?;
        let RuntimeResponse::Session(session) = response else {
            return Err(DaemonError::Runtime(format!(
                "participant worker returned {} for conversation provisioning",
                runtime_response_kind(&response)
            )));
        };
        if session.root_tree_id != root_tree_id
            || session.profile_id != *profile_id
            || session.archived
        {
            return Err(DaemonError::Runtime(
                "participant worker provisioned a conversation session outside its assigned profile root"
                    .into(),
            ));
        }
        Ok(session.session_id)
    }

    fn provision_group_participant_sessions(
        &mut self,
        route_session_id: Option<&SessionId>,
        conversation_id: &ConversationId,
        profile_ids: &[ProfileId],
    ) -> Result<Vec<SessionId>, DaemonError> {
        let mut expected = BTreeMap::new();
        for profile_id in profile_ids {
            if expected.contains_key(profile_id) {
                continue;
            }
            let root_tree_id = if let Some(manifest) = self
                .catalog
                .roots
                .values()
                .filter(|manifest| {
                    manifest.profile_id == *profile_id && manifest.state != SessionState::Archived
                })
                .max_by_key(|manifest| manifest.updated_at)
            {
                manifest.root_tree_id.clone()
            } else {
                self.profile_runtime_root(profile_id)?.0
            };
            expected.insert(profile_id.clone(), root_tree_id);
        }
        if expected.is_empty() {
            return Err(DaemonError::Runtime(
                "group creation returned no participant profiles to provision".into(),
            ));
        }
        let assignments = expected
            .iter()
            .map(|(profile_id, root_tree_id)| ConversationSessionAssignment {
                profile_id: profile_id.clone(),
                root_tree_id: root_tree_id.clone(),
            })
            .collect::<Vec<_>>();
        let (route_root, status) = self.runtime_route(route_session_id)?;
        let response = self.execute_worker(
            &route_root,
            status.generation,
            RuntimeRequest::ProvisionConversationSessions {
                conversation_id: conversation_id.clone(),
                assignments,
                generation: status.generation,
                now: UtcTimestamp::now().map_err(|error| {
                    DaemonError::Runtime(format!(
                        "group participant provisioning clock is unavailable: {error}"
                    ))
                })?,
            },
        )?;
        let RuntimeResponse::Sessions(sessions) = response else {
            return Err(DaemonError::Runtime(format!(
                "group coordinator worker returned {} for participant provisioning",
                runtime_response_kind(&response)
            )));
        };
        if sessions.len() != expected.len() {
            return Err(DaemonError::Runtime(
                "group coordinator worker returned an incomplete participant session batch".into(),
            ));
        }
        let mut session_ids = Vec::with_capacity(sessions.len());
        let mut returned_profiles = BTreeSet::new();
        for session in sessions {
            if session.archived
                || !returned_profiles.insert(session.profile_id.clone())
                || expected.get(&session.profile_id) != Some(&session.root_tree_id)
            {
                return Err(DaemonError::Runtime(
                    "group coordinator worker provisioned a participant session outside its assigned profile root"
                        .into(),
                ));
            }
            let summary = SessionSummary {
                session_id: session.session_id.clone(),
                root_tree_id: session.root_tree_id,
                profile_id: session.profile_id,
                title: session.title,
                state: SessionState::Dormant,
                updated_at: session.created_at,
            };
            self.register_runtime_session_alias(&summary)?;
            session_ids.push(summary.session_id);
        }
        if returned_profiles.len() != expected.len() {
            return Err(DaemonError::Runtime(
                "group coordinator worker omitted an assigned participant profile".into(),
            ));
        }
        Ok(session_ids)
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
        requester_authority: RuntimeCommandAuthority,
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
        let worker_binding = self.runtime_worker_binding(&root, &status)?;
        match self.execute_worker(
            &root,
            status.generation,
            RuntimeRequest::ExecuteFeature {
                client_id: client_id.clone(),
                requester_authority,
                worker_binding,
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

    fn runtime_worker_binding(
        &self,
        root_tree_id: &RootTreeId,
        status: &WorkerStatus,
    ) -> Result<RuntimeWorkerBinding, DaemonError> {
        let lease = LeaseManager::open(self.supervisor.lease_database_path())?
            .current(root_tree_id)?
            .ok_or_else(|| {
                DaemonError::Runtime("routed worker lease is missing or expired".into())
            })?;
        if lease.worker_id != status.worker_id || lease.generation != status.generation {
            return Err(DaemonError::Runtime(
                "routed worker status does not match its current lease".into(),
            ));
        }
        Ok(RuntimeWorkerBinding {
            root_tree_id: lease.root_tree_id,
            worker_id: lease.worker_id,
            generation: lease.generation,
            lease_authentication: lease.authentication,
        })
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
        if let Some(root) = self.catalog.root_for_session(&prompt.session_id).cloned()
            && let Some(hub) = self.event_hubs.get_mut(&root)
            && let Ok(envelope) = hub.publish(DaemonEvent::CommandAccepted {
                command_id: accepted.accepted.acceptance_id.clone(),
            })
        {
            events(envelope);
        }
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

#[cfg(debug_assertions)]
pub(crate) fn startup_fault_requested(boundary: &str) -> bool {
    std::env::var("KEITH_DAEMON_STARTUP_FAIL_AT").as_deref() == Ok(boundary)
        && std::env::var("KEITH_DAEMON_STARTUP_FAIL_IMAGE").ok()
            == std::env::var("KEITH_DAEMON_READY_IMAGE").ok()
}

#[cfg(not(debug_assertions))]
pub(crate) const fn startup_fault_requested(_boundary: &str) -> bool {
    false
}

const fn runtime_response_kind(response: &RuntimeResponse) -> &'static str {
    match response {
        RuntimeResponse::Profiles(_) => "profiles",
        RuntimeResponse::Sessions(_) => "sessions",
        RuntimeResponse::Session(_) => "session",
        RuntimeResponse::Snapshot(_) => "snapshot",
        RuntimeResponse::Command(_) => "command",
        RuntimeResponse::CandidateCanary(_) => "candidate_canary",
        RuntimeResponse::Complete => "complete",
        RuntimeResponse::Failed(_) => "failed",
    }
}

fn command_result_conversation_id(result: &CommandResult) -> Option<ConversationId> {
    let CommandResult::Data(payload) = result else {
        return None;
    };
    let ResponsePayload::TeammatesReceipt(receipt) = payload.as_ref() else {
        return None;
    };
    receipt.conversation_id.clone()
}

fn peer_message_provisioning_authorized(
    catalog: &RootCatalog,
    authority: &RuntimeCommandAuthority,
    scope_session_id: Option<&SessionId>,
    request: &keith_protocol::PeerMessageCommand,
) -> bool {
    let RuntimeCommandAuthority::Agent {
        profile_id,
        session_id,
    } = authority
    else {
        return false;
    };
    if profile_id != &request.sender_profile_id || Some(session_id) != scope_session_id {
        return false;
    }
    catalog
        .root_for_session(&request.participant_session_id)
        .and_then(|root_tree_id| catalog.root(root_tree_id))
        .is_some_and(|manifest| {
            manifest.profile_id == request.recipient_profile_id
                && manifest.state != SessionState::Archived
        })
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

const fn command_rejects_session_scope(command: &ClientCommand) -> bool {
    matches!(
        command,
        ClientCommand::AgentLifecycle(_) | ClientCommand::Computer(_)
    )
}

fn command_route_session_id<'a>(
    scope_session_id: Option<&'a SessionId>,
    embedded_session_id: Option<&'a SessionId>,
    command: &ClientCommand,
) -> Option<&'a SessionId> {
    if matches!(
        command,
        ClientCommand::AgentLifecycle(_) | ClientCommand::Computer(_)
    ) {
        None
    } else if command_supports_descendant_target(command) {
        scope_session_id.or(embedded_session_id)
    } else {
        embedded_session_id.or(scope_session_id)
    }
}

fn effective_requester_scope<'a>(
    attached_scope: Option<&'a SessionId>,
    envelope_scope: Option<&'a SessionId>,
) -> Option<&'a SessionId> {
    attached_scope.or(envelope_scope)
}

fn runtime_command_authority(
    catalog: &RootCatalog,
    requester_scope: Option<&SessionId>,
) -> Result<RuntimeCommandAuthority, DaemonError> {
    let Some(session_id) = requester_scope else {
        return Ok(RuntimeCommandAuthority::HumanOwner);
    };
    let root = catalog
        .root_for_session(session_id)
        .ok_or_else(|| DaemonError::UnknownSession(session_id.clone()))?;
    let profile_id = catalog
        .root(root)
        .ok_or_else(|| DaemonError::UnknownSession(session_id.clone()))?
        .profile_id
        .clone();
    Ok(RuntimeCommandAuthority::Agent {
        profile_id,
        session_id: session_id.clone(),
    })
}

fn rejected_runtime(error: String) -> (CommandResult, Vec<keith_protocol::EventEnvelope>) {
    rejected_daemon(DaemonError::Runtime(error))
}

fn rejected_evolution(
    code: ErrorCode,
    message: &str,
) -> (CommandResult, Vec<keith_protocol::EventEnvelope>) {
    (
        CommandResult::Rejected(CommandError {
            error: CommonError::new(code, message, false),
            unsupported_feature: None,
        }),
        Vec::new(),
    )
}

fn record_evolution_approval(
    controller: &EvolutionController,
    hypothesis_id: EntityId,
    now: UtcTimestamp,
) -> Result<(), String> {
    let event = EvolutionEvent::Consent {
        hypothesis_id: hypothesis_id.clone(),
        approved: true,
        acting_identity: LedgerText::redacted("installation-owner", 256, &[])
            .map_err(|error| error.to_string())?,
    };
    controller
        .ledger
        .append_checked(EntityId::new(), now, event, |records| {
            if verified_hypothesis_state(records, &hypothesis_id)?
                != Some(HypothesisState::AwaitingApproval)
            {
                return Err(keith_self_evolution::LedgerError::Store(
                    "hypothesis is not awaiting explicit human approval".into(),
                ));
            }
            if records.iter().any(|record| {
                matches!(&record.event, EvolutionEvent::Consent { hypothesis_id: id, .. } if id == &hypothesis_id)
            }) {
                return Err(keith_self_evolution::LedgerError::Store(
                    "a consent decision is already recorded for this hypothesis".into(),
                ));
            }
            Ok(())
        })
        .map(|_| ())
        .map_err(|error| error.to_string())
}

fn verified_hypothesis_state(
    records: &[keith_self_evolution::EvolutionRecord],
    hypothesis_id: &EntityId,
) -> Result<Option<HypothesisState>, keith_self_evolution::LedgerError> {
    let mut current: Option<(HypothesisState, u64, UtcTimestamp)> = None;
    for record in records {
        match &record.event {
            EvolutionEvent::Hypothesis { hypothesis } if &hypothesis.id == hypothesis_id => {
                if current.is_some() {
                    return Err(keith_self_evolution::LedgerError::Quarantined(
                        "duplicate hypothesis identity".into(),
                    ));
                }
                current = Some((HypothesisState::Proposed, 0, record.occurred_at));
            }
            EvolutionEvent::Admission {
                hypothesis_id: id,
                admitted,
                ..
            } if id == hypothesis_id => {
                let Some((HypothesisState::Proposed, 0, updated_at)) = current else {
                    if !admitted && current.is_none() {
                        continue;
                    }
                    return Err(keith_self_evolution::LedgerError::Quarantined(
                        "admission is out of order".into(),
                    ));
                };
                if record.occurred_at < updated_at {
                    return Err(keith_self_evolution::LedgerError::Quarantined(
                        "hypothesis timestamp regressed".into(),
                    ));
                }
                current = Some((
                    if *admitted {
                        HypothesisState::Admitted
                    } else {
                        HypothesisState::Rejected
                    },
                    1,
                    record.occurred_at,
                ));
            }
            EvolutionEvent::HypothesisTransition {
                hypothesis_id: id,
                from,
                to,
                revision,
                ..
            } if id == hypothesis_id => {
                let Some((state, current_revision, updated_at)) = current else {
                    return Err(keith_self_evolution::LedgerError::Quarantined(
                        "transition precedes hypothesis".into(),
                    ));
                };
                if state != *from
                    || current_revision.checked_add(1) != Some(*revision)
                    || !valid_hypothesis_transition(*from, *to)
                    || record.occurred_at < updated_at
                {
                    return Err(keith_self_evolution::LedgerError::Quarantined(
                        "invalid hypothesis transition chain".into(),
                    ));
                }
                current = Some((*to, *revision, record.occurred_at));
            }
            _ => {}
        }
    }
    Ok(current.map(|(state, _, _)| state))
}

const fn valid_hypothesis_transition(from: HypothesisState, to: HypothesisState) -> bool {
    use HypothesisState as State;
    matches!(
        (from, to),
        (
            State::Proposed,
            State::Admitted | State::Rejected | State::Expired
        ) | (
            State::Admitted,
            State::Proposing | State::Rejected | State::Expired
        ) | (
            State::Proposing,
            State::Verifying | State::Failed | State::Expired
        ) | (
            State::Verifying,
            State::Evaluating | State::Failed | State::Expired
        ) | (
            State::Evaluating,
            State::AwaitingApproval | State::Promoting | State::Failed | State::Expired
        ) | (
            State::AwaitingApproval,
            State::Promoting | State::Rejected | State::Expired
        ) | (State::Promoting, State::Promoted | State::Failed)
            | (State::Promoted, State::Observing)
            | (
                State::Observing,
                State::Promoted | State::Reverted | State::Failed
            )
    )
}

fn reverse_evolution(
    supervisor: &mut WorkerSupervisor,
    data_root: &Path,
    controller: &EvolutionController,
    scope: ReversalScope,
    reason: &str,
    now: UtcTimestamp,
) -> Result<(), String> {
    let authority = controller
        .enablement
        .authorize_reversal(&controller.authority)
        .map_err(|error| error.to_string())?;
    let transaction = ReversalTransaction::open(
        data_root.join("self-evolution").join("promotions"),
        &controller.source_root,
    )
    .map_err(|error| error.to_string())?;
    transaction
        .reverse(
            supervisor,
            &controller.ledger,
            keith_self_evolution::ReversalRequest {
                scope,
                trusted_public_key: controller.ledger.trusted_public_key(),
                authority: &authority,
                reason,
                occurred_at: now,
            },
        )
        .map(|_| ())
        .map_err(|error| error.to_string())
}

fn evolution_projection(
    controller: &EvolutionController,
    before_sequence: Option<u64>,
    limit: usize,
) -> Result<EvolutionProjection, String> {
    let availability = match keith_self_evolution::probe_availability(&controller.source_root) {
        keith_self_evolution::EvolutionAvailability::Available { rustc, cargo } => {
            EvolutionAvailabilityProjection::Available { rustc, cargo }
        }
        keith_self_evolution::EvolutionAvailability::Unavailable(reasons) => {
            EvolutionAvailabilityProjection::Unavailable {
                reasons: reasons
                    .into_iter()
                    .map(|reason| format!("{reason:?}"))
                    .collect(),
            }
        }
    };
    let records = controller
        .ledger
        .records()
        .map_err(|error| error.to_string())?;
    let eligible = records
        .iter()
        .rev()
        .filter(|record| before_sequence.is_none_or(|before| record.sequence < before));
    let available = eligible.clone().count();
    let mut ledger: Vec<EvolutionLedgerProjection> = eligible
        .take(limit)
        .map(evolution_ledger_projection)
        .collect();
    for row in &mut ledger {
        if let Some(id) = row.hypothesis_id.clone() {
            enrich_evolution_row(row, &id, &records);
        }
    }
    let active = active_evolution_hypothesis(&records)?;
    Ok(EvolutionProjection {
        protocol_version: CURRENT_PROTOCOL_VERSION,
        enabled: controller.enablement.enabled(),
        state: if controller.enablement.enabled() { "enabled" } else { "disabled" }.into(),
        availability,
        disclosure: EvolutionDisclosureProjection {
            editable_surface: "bounded, guard-approved Rust source outside the protected surface".into(),
            protected_surface: "agent loop, personal memory, evolution guard and policy, evolution ledger, and release verification".into(),
            autonomy: "tests and non-shipping material may proceed after verification; binary and daemon changes use stronger consent".into(),
            reversal: "every promoted change remains individually reversible, including while evolution is disabled".into(),
        },
        active,
        has_more_ledger: available > limit,
        ledger,
        guidance: (!controller.enablement.enabled()).then(|| "Self-evolution is disabled. Enablement is installation-owner-only and is not accepted from any client command.".into()),
    })
}

fn enrich_evolution_row(
    row: &mut EvolutionLedgerProjection,
    hypothesis_id: &EntityId,
    records: &[keith_self_evolution::EvolutionRecord],
) {
    for record in records {
        match &record.event {
            EvolutionEvent::Hypothesis { hypothesis } if &hypothesis.id == hypothesis_id => {
                row.evidence = vec![format!(
                    "{} measured by {} from {} recorded evidence item(s)",
                    hypothesis.target_subsystem.as_str(),
                    hypothesis.metric.as_str(),
                    hypothesis.evidence_refs.len()
                )];
            }
            EvolutionEvent::Proposal {
                hypothesis_id: id,
                readable_diff,
            } if id == hypothesis_id => {
                row.readable_diff = Some(readable_diff.as_str().into());
            }
            EvolutionEvent::Canary {
                hypothesis_id: id,
                before,
                after,
                ..
            }
            | EvolutionEvent::Observation {
                hypothesis_id: id,
                before,
                after,
                ..
            } if id == hypothesis_id => {
                row.measured_result = Some(format!("{before} -> {after}"));
            }
            EvolutionEvent::HypothesisTransition {
                hypothesis_id: id,
                to,
                ..
            } if id == hypothesis_id => {
                row.state = hypothesis_state_label(*to).into();
            }
            _ => {}
        }
    }
}

fn active_evolution_hypothesis(
    records: &[keith_self_evolution::EvolutionRecord],
) -> Result<Option<EvolutionHypothesisProjection>, String> {
    let mut seen = BTreeSet::new();
    let mut active = None;
    for record in records.iter().rev() {
        let id = match &record.event {
            EvolutionEvent::Hypothesis { hypothesis } => Some(hypothesis.id.clone()),
            EvolutionEvent::Admission { hypothesis_id, .. }
            | EvolutionEvent::HypothesisTransition { hypothesis_id, .. }
            | EvolutionEvent::Proposal { hypothesis_id, .. }
            | EvolutionEvent::Gate { hypothesis_id, .. }
            | EvolutionEvent::Canary { hypothesis_id, .. }
            | EvolutionEvent::Consent { hypothesis_id, .. }
            | EvolutionEvent::Promotion { hypothesis_id, .. }
            | EvolutionEvent::Observation { hypothesis_id, .. }
            | EvolutionEvent::Revert { hypothesis_id, .. } => Some(hypothesis_id.clone()),
            _ => None,
        };
        let Some(id) = id else { continue };
        if !seen.insert(id.clone()) {
            continue;
        }
        let Some(state) =
            verified_hypothesis_state(records, &id).map_err(|error| error.to_string())?
        else {
            continue;
        };
        if !matches!(
            state,
            HypothesisState::Promoted
                | HypothesisState::Reverted
                | HypothesisState::Rejected
                | HypothesisState::Expired
                | HypothesisState::Failed
        ) {
            active = Some((id, state));
            break;
        }
    }
    let Some((id, state)) = active else {
        return Ok(None);
    };
    let hypothesis = records.iter().find_map(|record| match &record.event {
        EvolutionEvent::Hypothesis { hypothesis } if hypothesis.id == id => Some(hypothesis),
        _ => None,
    });
    let Some(hypothesis) = hypothesis else {
        return Ok(None);
    };
    let approval_recorded = records.iter().any(|record| {
        matches!(
            &record.event,
            EvolutionEvent::Consent {
                hypothesis_id,
                approved: true,
                ..
            } if hypothesis_id == &id
        )
    });
    let mut projection = EvolutionHypothesisProjection {
        hypothesis_id: id.clone(),
        target: hypothesis.target_subsystem.as_str().into(),
        metric: hypothesis.metric.as_str().into(),
        state: if state == HypothesisState::AwaitingApproval && approval_recorded {
            "approval-recorded"
        } else {
            hypothesis_state_label(state)
        }
        .into(),
        evidence: vec![format!(
            "{} recorded evidence item(s)",
            hypothesis.evidence_refs.len()
        )],
        measured_result: None,
        readable_diff: None,
        approval_required: state == HypothesisState::AwaitingApproval && !approval_recorded,
    };
    for record in records {
        match &record.event {
            EvolutionEvent::Proposal {
                hypothesis_id,
                readable_diff,
            } if hypothesis_id == &id => {
                projection.readable_diff = Some(readable_diff.as_str().into());
            }
            EvolutionEvent::Canary {
                hypothesis_id,
                before,
                after,
                ..
            }
            | EvolutionEvent::Observation {
                hypothesis_id,
                before,
                after,
                ..
            } if hypothesis_id == &id => {
                projection.measured_result = Some(format!("{before} -> {after}"));
            }
            _ => {}
        }
    }
    Ok(Some(projection))
}

const fn hypothesis_state_label(state: HypothesisState) -> &'static str {
    match state {
        HypothesisState::Proposed => "proposed",
        HypothesisState::Admitted => "admitted",
        HypothesisState::Proposing => "proposing",
        HypothesisState::Verifying => "verifying",
        HypothesisState::Evaluating => "evaluating",
        HypothesisState::AwaitingApproval => "awaiting-approval",
        HypothesisState::Promoting => "promoting",
        HypothesisState::Promoted => "promoted",
        HypothesisState::Observing => "observing",
        HypothesisState::Reverted => "reverted",
        HypothesisState::Rejected => "rejected",
        HypothesisState::Expired => "expired",
        HypothesisState::Failed => "failed",
    }
}

fn evolution_ledger_projection(
    record: &keith_self_evolution::EvolutionRecord,
) -> EvolutionLedgerProjection {
    let mut hypothesis_id = None;
    let mut promotion_id = None;
    let mut evidence = Vec::new();
    let mut measured_result = None;
    let mut readable_diff = None;
    let (kind, summary, state, reversible) = match &record.event {
        EvolutionEvent::Hypothesis { hypothesis } => {
            hypothesis_id = Some(hypothesis.id.clone());
            evidence = hypothesis
                .evidence_refs
                .iter()
                .map(ToString::to_string)
                .collect();
            (
                "hypothesis",
                format!(
                    "Proposed improvement to {} measured by {}",
                    hypothesis.target_subsystem.as_str(),
                    hypothesis.metric.as_str()
                ),
                "proposed",
                false,
            )
        }
        EvolutionEvent::HypothesisTransition {
            hypothesis_id: id,
            to,
            ..
        } => {
            hypothesis_id = Some(id.clone());
            (
                "state",
                "Evolution state changed".into(),
                match to {
                    HypothesisState::AwaitingApproval => "awaiting-approval",
                    HypothesisState::Promoted => "promoted",
                    HypothesisState::Reverted => "reverted",
                    HypothesisState::Failed => "failed",
                    HypothesisState::Rejected => "rejected",
                    HypothesisState::Expired => "expired",
                    _ => "in-progress",
                },
                false,
            )
        }
        EvolutionEvent::Proposal {
            hypothesis_id: id,
            readable_diff: diff,
        } => {
            hypothesis_id = Some(id.clone());
            readable_diff = Some(diff.as_str().into());
            (
                "proposal",
                "Source change proposed".into(),
                "proposing",
                false,
            )
        }
        EvolutionEvent::Canary {
            hypothesis_id: id,
            before,
            after,
            passed,
        }
        | EvolutionEvent::Observation {
            hypothesis_id: id,
            before,
            after,
            healthy: passed,
        } => {
            hypothesis_id = Some(id.clone());
            measured_result = Some(format!("{before} -> {after}"));
            (
                "measurement",
                "Measured candidate result".into(),
                if *passed { "passed" } else { "failed" },
                false,
            )
        }
        EvolutionEvent::Consent {
            hypothesis_id: id,
            approved,
            ..
        } => {
            hypothesis_id = Some(id.clone());
            (
                "consent",
                "Installation owner recorded a consent decision".into(),
                if *approved { "approved" } else { "declined" },
                false,
            )
        }
        EvolutionEvent::Promotion {
            hypothesis_id: id,
            promotion_id: promotion,
            ..
        } => {
            hypothesis_id = Some(id.clone());
            promotion_id = Some(promotion.clone());
            (
                "promotion",
                "Verified change promoted".into(),
                "promoted",
                true,
            )
        }
        EvolutionEvent::Revert {
            hypothesis_id: id,
            promotion_ids,
            ..
        } => {
            hypothesis_id = Some(id.clone());
            promotion_id = promotion_ids.first().cloned();
            (
                "revert",
                "Promoted change reversed".into(),
                "reverted",
                false,
            )
        }
        EvolutionEvent::Enable { .. } => (
            "enable",
            "Self-evolution enabled by installation owner".into(),
            "enabled",
            false,
        ),
        EvolutionEvent::Disable { .. } => (
            "disable",
            "Self-evolution disabled by installation owner".into(),
            "disabled",
            false,
        ),
        other => (
            "audit",
            format!(
                "{} recorded",
                match other {
                    EvolutionEvent::Admission { .. } => "Admission decision",
                    EvolutionEvent::EvidenceAttestation { .. } => "Evidence",
                    EvolutionEvent::Gate { .. } => "Verification gate",
                    _ => "Evolution event",
                }
            ),
            "recorded",
            false,
        ),
    };
    EvolutionLedgerProjection {
        sequence: record.sequence,
        occurred_at: record.occurred_at,
        kind: kind.into(),
        summary,
        state: state.into(),
        evidence,
        measured_result,
        readable_diff,
        hypothesis_id,
        promotion_id,
        reversible,
    }
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

    #[test]
    fn daemon_evolution_secret_is_stable_and_rejects_unsafe_existing_paths() {
        let directory = tempfile::tempdir().unwrap();
        let secret_path = directory.path().join("authority.seed");
        let first = read_or_create_secret(&secret_path).unwrap();
        assert_eq!(read_or_create_secret(&secret_path).unwrap(), first);
        #[cfg(unix)]
        {
            use std::os::unix::fs::{MetadataExt, PermissionsExt, symlink};
            assert_eq!(fs::metadata(&secret_path).unwrap().mode() & 0o777, 0o600);
            fs::set_permissions(&secret_path, fs::Permissions::from_mode(0o644)).unwrap();
            assert_eq!(
                read_or_create_secret(&secret_path).unwrap_err().kind(),
                io::ErrorKind::PermissionDenied
            );
            let target = directory.path().join("target");
            fs::write(&target, [0_u8; 32]).unwrap();
            let link = directory.path().join("link.seed");
            symlink(&target, &link).unwrap();
            assert_eq!(
                read_or_create_secret(&link).unwrap_err().kind(),
                io::ErrorKind::InvalidData
            );
        }
    }

    fn evolution_test_daemon(root: &Path) -> DaemonCore {
        let source_root = root.join("source");
        fs::create_dir_all(&source_root).unwrap();
        DaemonCore::open(
            root.join("data"),
            std::env::current_exe().unwrap(),
            DaemonOptions {
                evolution_source_root: Some(source_root),
                ..DaemonOptions::default()
            },
        )
        .unwrap()
    }

    fn evolution_data(result: CommandResult) -> EvolutionProjection {
        let CommandResult::Data(payload) = result else {
            panic!("expected authoritative evolution projection");
        };
        let ResponsePayload::Evolution(projection) = *payload else {
            panic!("expected evolution payload");
        };
        *projection
    }

    fn ledger_text(value: &str) -> LedgerText {
        LedgerText::redacted(value, 1024, &[]).unwrap()
    }

    fn append_awaiting_hypothesis(controller: &EvolutionController, hypothesis_id: &EntityId) {
        let mut timestamp = 10_i64;
        controller
            .ledger
            .append(
                EntityId::new(),
                UtcTimestamp::from_unix_millis(timestamp),
                EvolutionEvent::Hypothesis {
                    hypothesis: keith_self_evolution::LedgerHypothesis {
                        id: hypothesis_id.clone(),
                        evidence_refs: vec![EntityId::new()],
                        target_subsystem: ledger_text("tool routing"),
                        metric: ledger_text("turn failure rate"),
                        baseline: 0.2,
                        target: 0.1,
                        revert_threshold: 0.25,
                        expires_at: UtcTimestamp::from_unix_millis(4_000_000_000_000),
                        measurement_slice: None,
                        evidence_sources: Vec::new(),
                        evidence_digests: Vec::new(),
                    },
                },
            )
            .unwrap();
        timestamp += 1;
        controller
            .ledger
            .append(
                EntityId::new(),
                UtcTimestamp::from_unix_millis(timestamp),
                EvolutionEvent::Admission {
                    hypothesis_id: hypothesis_id.clone(),
                    admitted: true,
                    reason: ledger_text("host evidence resolved"),
                },
            )
            .unwrap();
        let transitions = [
            (HypothesisState::Admitted, HypothesisState::Proposing, 2),
            (HypothesisState::Proposing, HypothesisState::Verifying, 3),
            (HypothesisState::Verifying, HypothesisState::Evaluating, 4),
            (
                HypothesisState::Evaluating,
                HypothesisState::AwaitingApproval,
                5,
            ),
        ];
        for (from, to, revision) in transitions {
            timestamp += 1;
            controller
                .ledger
                .append(
                    EntityId::new(),
                    UtcTimestamp::from_unix_millis(timestamp),
                    EvolutionEvent::HypothesisTransition {
                        hypothesis_id: hypothesis_id.clone(),
                        from,
                        to,
                        revision,
                        reason: None,
                    },
                )
                .unwrap();
        }
        controller
            .ledger
            .append(
                EntityId::new(),
                UtcTimestamp::from_unix_millis(timestamp + 1),
                EvolutionEvent::Proposal {
                    hypothesis_id: hypothesis_id.clone(),
                    readable_diff: ledger_text("Stop after the first verified matching route"),
                },
            )
            .unwrap();
        controller
            .ledger
            .append(
                EntityId::new(),
                UtcTimestamp::from_unix_millis(timestamp + 2),
                EvolutionEvent::Canary {
                    hypothesis_id: hypothesis_id.clone(),
                    before: 0.2,
                    after: 0.08,
                    passed: true,
                },
            )
            .unwrap();
    }

    #[test]
    fn evolution_commands_enforce_owner_enablement_and_use_real_domain_mutations() {
        let directory = tempfile::tempdir().unwrap();
        let mut daemon = evolution_test_daemon(directory.path());
        let before = daemon.evolution.ledger.records().unwrap().len();
        let (enable, _) = daemon.execute_evolution_command(EvolutionCommand::Enable {
            disclosure_acknowledged: true,
        });
        let CommandResult::Rejected(enable) = enable else {
            panic!("client enablement must be rejected");
        };
        assert_eq!(enable.error.code, ErrorCode::Unauthorized);
        assert!(enable.error.message.contains("installation-owner control"));
        assert_eq!(daemon.evolution.ledger.records().unwrap().len(), before);

        let (disabled, _) = daemon.execute_evolution_command(EvolutionCommand::Disable {
            reason: "owner paused evolution from the terminal".into(),
        });
        assert!(!evolution_data(disabled).enabled);
        assert!(matches!(
            daemon.evolution.ledger.records().unwrap().last().map(|row| &row.event),
            Some(EvolutionEvent::Disable { reason, .. })
                if reason.as_str() == "owner paused evolution from the terminal"
        ));

        let hypothesis_id = EntityId::new();
        append_awaiting_hypothesis(&daemon.evolution, &hypothesis_id);
        let (approved, _) = daemon.execute_evolution_command(EvolutionCommand::Approve {
            hypothesis_id: hypothesis_id.clone(),
        });
        let approved = evolution_data(approved);
        let active = approved
            .active
            .expect("awaiting hypothesis remains visible");
        assert_eq!(active.hypothesis_id, hypothesis_id);
        assert_eq!(active.state, "approval-recorded");
        assert!(!active.approval_required);
        assert!(matches!(
            daemon
                .evolution
                .ledger
                .records()
                .unwrap()
                .last()
                .map(|row| &row.event),
            Some(EvolutionEvent::Consent { approved: true, .. })
        ));
        let (duplicate, _) =
            daemon.execute_evolution_command(EvolutionCommand::Approve { hypothesis_id });
        assert!(
            matches!(duplicate, CommandResult::Rejected(error) if error.error.code == ErrorCode::Conflict)
        );

        let (reversal, _) = daemon.execute_evolution_command(EvolutionCommand::Revert {
            promotion_id: EntityId::new(),
            reason: "verify the real reversal boundary".into(),
        });
        assert!(matches!(
            reversal,
            CommandResult::Rejected(error)
                if error.error.code == ErrorCode::Conflict
                    && error.error.message.contains("no committed promotion matches")
        ));
    }

    #[test]
    fn evolution_browse_is_exclusive_bounded_and_enriches_reversible_rows() {
        let directory = tempfile::tempdir().unwrap();
        let mut daemon = evolution_test_daemon(directory.path());
        let hypothesis_id = EntityId::new();
        append_awaiting_hypothesis(&daemon.evolution, &hypothesis_id);
        let promotion_id = EntityId::new();
        daemon
            .evolution
            .ledger
            .append(
                EntityId::new(),
                UtcTimestamp::from_unix_millis(20),
                EvolutionEvent::Promotion {
                    hypothesis_id,
                    promotion_id: promotion_id.clone(),
                    artifact_id: ledger_text("worker-image"),
                    artifact_digest: ledger_text(&"a".repeat(64)),
                },
            )
            .unwrap();
        let last_sequence = daemon
            .evolution
            .ledger
            .records()
            .unwrap()
            .last()
            .unwrap()
            .sequence;

        let (latest, _) = daemon.execute_evolution_command(EvolutionCommand::BrowseLedger {
            before_sequence: None,
            limit: 1,
        });
        let latest = evolution_data(latest);
        assert_eq!(latest.ledger.len(), 1);
        assert_eq!(latest.ledger[0].sequence, last_sequence);
        assert_eq!(latest.ledger[0].promotion_id, Some(promotion_id));
        assert!(latest.ledger[0].reversible);
        assert!(!latest.ledger[0].evidence.is_empty());
        assert!(latest.ledger[0].readable_diff.is_some());
        assert_eq!(
            latest.ledger[0].measured_result.as_deref(),
            Some("0.2 -> 0.08")
        );
        assert!(latest.has_more_ledger);

        let (previous, _) = daemon.execute_evolution_command(EvolutionCommand::BrowseLedger {
            before_sequence: Some(last_sequence),
            limit: 1,
        });
        let previous = evolution_data(previous);
        assert_eq!(previous.ledger.len(), 1);
        assert_eq!(previous.ledger[0].sequence, last_sequence - 1);
        assert!(previous.has_more_ledger);
    }

    #[test]
    fn signed_enablement_state_is_restored_when_the_daemon_reopens() {
        let directory = tempfile::tempdir().unwrap();
        let root = directory.path();
        let daemon = evolution_test_daemon(root);
        daemon
            .evolution
            .ledger
            .append(
                EntityId::new(),
                UtcTimestamp::from_unix_millis(1),
                EvolutionEvent::Enable {
                    acting_identity: ledger_text("installation-owner"),
                },
            )
            .unwrap();
        drop(daemon);

        let mut reopened = evolution_test_daemon(root);
        let (status, _) = reopened.execute_evolution_command(EvolutionCommand::Status);
        let status = evolution_data(status);
        assert!(status.enabled);
        assert_eq!(status.state, "enabled");
    }

    fn manifest(root: RootTreeId, session: SessionId) -> RootManifest {
        RootManifest {
            version: CURRENT_SCHEMA_VERSION,
            root_tree_id: root,
            root_session_id: session,
            session_aliases: Vec::new(),
            profile_id: ProfileId::new(),
            title: Some("catalog entry".into()),
            state: SessionState::Dormant,
            updated_at: UtcTimestamp::UNIX_EPOCH,
        }
    }

    #[test]
    fn peer_provisioning_requires_exact_agent_origin_and_recipient_root_anchor() {
        let sender_profile = ProfileId::new();
        let sender_session = SessionId::new();
        let recipient_profile = ProfileId::new();
        let recipient_session = SessionId::new();
        let mut recipient = manifest(RootTreeId::new(), recipient_session.clone());
        recipient.profile_id = recipient_profile.clone();
        let mut catalog = RootCatalog::default();
        catalog.insert(recipient).unwrap();
        let request = keith_protocol::PeerMessageCommand {
            request_id: EntityId::new(),
            operation_key: StableKey::parse("peer-broker-authority").unwrap(),
            conversation_id: ConversationId::new(),
            sender_profile_id: sender_profile.clone(),
            recipient_profile_id: recipient_profile,
            participant_session_id: recipient_session,
            expected_conversation_revision: Revision::ZERO,
            expected_policy_revision: Revision::ZERO,
            content: "hello".into(),
            deadline: UtcTimestamp::from_unix_millis(i64::MAX),
        };
        let authority = RuntimeCommandAuthority::Agent {
            profile_id: sender_profile,
            session_id: sender_session.clone(),
        };
        assert!(peer_message_provisioning_authorized(
            &catalog,
            &authority,
            Some(&sender_session),
            &request,
        ));
        assert!(!peer_message_provisioning_authorized(
            &catalog,
            &RuntimeCommandAuthority::HumanOwner,
            None,
            &request,
        ));
        let wrong_scope = SessionId::new();
        assert!(!peer_message_provisioning_authorized(
            &catalog,
            &authority,
            Some(&wrong_scope),
            &request,
        ));
        let mut wrong_sender = request.clone();
        wrong_sender.sender_profile_id = ProfileId::new();
        assert!(!peer_message_provisioning_authorized(
            &catalog,
            &authority,
            Some(&sender_session),
            &wrong_sender,
        ));
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
    fn discovery_restores_durable_branch_aliases_from_the_root_manifest() {
        let directory = tempfile::tempdir().unwrap();
        let root = RootTreeId::new();
        let root_session = SessionId::new();
        let branch_session = SessionId::new();
        let root_directory = directory.path().join("sessions").join(root.to_string());
        fs::create_dir_all(&root_directory).unwrap();
        let mut stored = manifest(root.clone(), root_session.clone());
        stored.session_aliases.push(SessionSummary {
            session_id: branch_session.clone(),
            root_tree_id: root.clone(),
            profile_id: stored.profile_id.clone(),
            title: Some("Conversation branch".into()),
            state: SessionState::Ready,
            updated_at: UtcTimestamp::from_unix_millis(42),
        });
        fs::write(
            root_directory.join("manifest.json"),
            keith_agent_types::canonical_json_bytes(&stored).unwrap(),
        )
        .unwrap();

        let catalog = RootCatalog::discover(directory.path()).unwrap();
        assert_eq!(catalog.root_for_session(&root_session), Some(&root));
        assert_eq!(catalog.root_for_session(&branch_session), Some(&root));
        assert_eq!(
            catalog
                .list(&SessionFilter::default())
                .into_iter()
                .map(|summary| summary.session_id)
                .collect::<BTreeSet<_>>(),
            BTreeSet::from([root_session, branch_session])
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

    #[test]
    fn teammate_routing_never_inherits_owner_admin_scope() {
        let attached_session = SessionId::new();
        let lifecycle = ClientCommand::AgentLifecycle(keith_protocol::AgentLifecycleCommand::List);
        assert!(command_rejects_session_scope(&lifecycle));
        assert_eq!(
            command_route_session_id(Some(&attached_session), None, &lifecycle),
            None
        );
        assert_eq!(
            effective_requester_scope(Some(&attached_session), None),
            Some(&attached_session)
        );

        let conversation =
            ClientCommand::Conversation(keith_protocol::ConversationCommand::Search(
                keith_protocol::ConversationSearchRequest {
                    query: "status".into(),
                    limit: 10,
                },
            ));
        assert!(!command_rejects_session_scope(&conversation));
        assert_eq!(
            command_route_session_id(Some(&attached_session), None, &conversation),
            Some(&attached_session)
        );
        assert_eq!(command_route_session_id(None, None, &conversation), None);

        let promotion = ClientCommand::Conversation(
            keith_protocol::ConversationCommand::PromoteConversationArtifact(
                keith_protocol::PromoteConversationArtifact {
                    artifact_id: keith_agent_types::ArtifactId::new(),
                    digest_sha256:
                        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa".into(),
                    conversation_id: keith_agent_types::ConversationId::new(),
                    expected_access_policy_revision: Revision::ZERO,
                    source_event_ids: vec![keith_agent_types::EventId::new()],
                    operation_key: "promote-1".into(),
                },
            ),
        );
        assert!(!command_rejects_session_scope(&promotion));
        assert_eq!(
            command_route_session_id(Some(&attached_session), None, &promotion),
            Some(&attached_session)
        );
    }

    #[test]
    fn requester_authority_preserves_human_owner_and_bound_agent_origins() {
        let root = RootTreeId::new();
        let session = SessionId::new();
        let entry = manifest(root, session.clone());
        let profile_id = entry.profile_id.clone();
        let mut catalog = RootCatalog::default();
        catalog.insert(entry).unwrap();
        assert_eq!(
            runtime_command_authority(&catalog, None).unwrap(),
            RuntimeCommandAuthority::HumanOwner
        );
        assert_eq!(
            runtime_command_authority(&catalog, effective_requester_scope(Some(&session), None))
                .unwrap(),
            RuntimeCommandAuthority::Agent {
                profile_id,
                session_id: session,
            }
        );
    }

    #[test]
    fn connection_close_discards_its_sticky_requester_scope() {
        let directory = tempfile::tempdir().unwrap();
        let mut daemon = evolution_test_daemon(directory.path());
        let client_id = keith_agent_types::ClientId::new();
        daemon
            .client_session_scopes
            .insert(client_id.clone(), SessionId::new());
        {
            let shared = Mutex::new(&mut daemon);
            let _cleanup = ClientScopeCleanup {
                shared: &shared,
                client_id: client_id.clone(),
            };
        }
        assert!(!daemon.client_session_scopes.contains_key(&client_id));
    }
}
