#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, File};
use std::path::PathBuf;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::mpsc::{Receiver, SyncSender, TrySendError, sync_channel};
use std::thread;

use keith_agent_types::{
    ActionId, CURRENT_SCHEMA_VERSION, ProfileId, Revision, RootTreeId, SchemaVersion, SessionId,
    UtcTimestamp,
};
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SessionRunState {
    Dormant,
    Ready,
    Running,
    WaitingTool,
    WaitingChild,
    WaitingExternal,
    Compacting,
    Paused,
    Failed,
    Archived,
}

impl SessionRunState {
    pub const ALL: [Self; 10] = [
        Self::Dormant,
        Self::Ready,
        Self::Running,
        Self::WaitingTool,
        Self::WaitingChild,
        Self::WaitingExternal,
        Self::Compacting,
        Self::Paused,
        Self::Failed,
        Self::Archived,
    ];

    pub const fn allows(self, next: Self) -> bool {
        if self as u8 == next as u8 {
            return true;
        }
        match self {
            Self::Dormant => matches!(next, Self::Ready | Self::Archived),
            Self::Ready => matches!(
                next,
                Self::Dormant
                    | Self::Running
                    | Self::Compacting
                    | Self::Paused
                    | Self::Failed
                    | Self::Archived
            ),
            Self::Running => matches!(
                next,
                Self::Ready
                    | Self::WaitingTool
                    | Self::WaitingChild
                    | Self::WaitingExternal
                    | Self::Compacting
                    | Self::Paused
                    | Self::Failed
            ),
            Self::WaitingTool | Self::WaitingChild => {
                matches!(
                    next,
                    Self::Running | Self::Ready | Self::Paused | Self::Failed
                )
            }
            Self::WaitingExternal => matches!(
                next,
                Self::Running | Self::Ready | Self::Paused | Self::Failed | Self::Archived
            ),
            Self::Compacting => {
                matches!(
                    next,
                    Self::Running | Self::Ready | Self::Paused | Self::Failed
                )
            }
            Self::Paused => matches!(next, Self::Ready | Self::Failed | Self::Archived),
            Self::Failed => matches!(next, Self::Ready | Self::Archived),
            Self::Archived => false,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SessionKind {
    Root,
    DurableChild,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SessionIdentity {
    pub kind: SessionKind,
    pub session_id: SessionId,
    pub root_tree_id: RootTreeId,
    pub parent_session_id: Option<SessionId>,
    pub profile_id: ProfileId,
}

impl SessionIdentity {
    pub fn root(session_id: SessionId, root_tree_id: RootTreeId, profile_id: ProfileId) -> Self {
        Self {
            kind: SessionKind::Root,
            session_id,
            root_tree_id,
            parent_session_id: None,
            profile_id,
        }
    }

    pub fn durable_child(
        session_id: SessionId,
        root_tree_id: RootTreeId,
        parent_session_id: SessionId,
        profile_id: ProfileId,
    ) -> Self {
        Self {
            kind: SessionKind::DurableChild,
            session_id,
            root_tree_id,
            parent_session_id: Some(parent_session_id),
            profile_id,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "message", content = "payload")]
pub enum ServiceMessage {
    InputAccepted { normalized: String },
    ActionQueued { action_id: ActionId },
    TurnPrepared { provider: String, model: String },
    ContextBuilt { content: String },
    ToolAuthorized { name: String },
    CompactionPrepared { target_tokens: usize },
    GoalValidated { objective: String },
    ChildValidated { child_session_id: SessionId },
    KernelPrepared { language: String },
    ExtensionResolved { key: String, value: String },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentSessionSnapshot {
    pub version: SchemaVersion,
    pub identity: SessionIdentity,
    pub state: SessionRunState,
    pub revision: Revision,
    pub updated_at: UtcTimestamp,
    pub last_service_message: Option<ServiceMessage>,
}

impl AgentSessionSnapshot {
    pub fn initial(identity: SessionIdentity) -> Self {
        Self {
            version: CURRENT_SCHEMA_VERSION,
            identity,
            state: SessionRunState::Dormant,
            revision: Revision::ZERO,
            updated_at: UtcTimestamp::UNIX_EPOCH,
            last_service_message: None,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum ServiceRequest {
    Input { text: String },
    EnqueueAction { summary: String },
    PrepareTurn,
    BuildContext { sections: Vec<String> },
    AuthorizeTool { name: String },
    PrepareCompaction { target_tokens: usize },
    ValidateGoal { objective: String },
    ValidateChild { objective: String },
    PrepareKernel { language: String },
    ResolveExtension { key: String },
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum SessionCommand {
    Transition(SessionRunState),
    Invoke(ServiceRequest),
    Snapshot,
    Shutdown,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum SessionReply {
    Transitioned(AgentSessionSnapshot),
    ServiceApplied {
        message: ServiceMessage,
        snapshot: AgentSessionSnapshot,
    },
    Snapshot(AgentSessionSnapshot),
    Shutdown(AgentSessionSnapshot),
}

#[derive(Clone, Debug, Eq, Error, PartialEq)]
pub enum ServiceError {
    #[error("service input is invalid: {0}")]
    InvalidInput(String),
    #[error("provider route is unavailable")]
    ProviderUnavailable,
    #[error("tool {0} is not authorized")]
    ToolUnauthorized(String),
    #[error("extension {0} does not exist")]
    ExtensionMissing(String),
    #[error("session persistence failed: {0}")]
    Persistence(String),
}

pub trait ProviderPort: Send + Sync {
    /// # Errors
    ///
    /// Returns a typed service error when no provider route is available.
    fn provider_route(&self, profile_id: &ProfileId) -> Result<(String, String), ServiceError>;
}

pub trait ToolPort: Send + Sync {
    fn tool_allowed(&self, name: &str) -> bool;
}

pub trait RepositoryPort: Send + Sync {
    /// # Errors
    ///
    /// Returns a typed service error when the snapshot cannot be loaded.
    fn load_snapshot(
        &self,
        session_id: &SessionId,
    ) -> Result<Option<AgentSessionSnapshot>, ServiceError>;

    /// # Errors
    ///
    /// Returns a typed service error when the snapshot cannot be persisted.
    fn store_snapshot(&self, snapshot: &AgentSessionSnapshot) -> Result<(), ServiceError>;
}

pub trait ChannelPort: Send + Sync {
    /// # Errors
    ///
    /// Returns a typed service error when the input is invalid.
    fn normalize_input(&self, input: &str) -> Result<String, ServiceError>;
}

pub trait ClientPort: Send + Sync {
    /// # Errors
    ///
    /// Returns a typed service error when the client text is invalid.
    fn validate_client_text(&self, input: &str) -> Result<(), ServiceError>;
}

pub trait InputPipeline: ChannelPort + ClientPort {
    /// # Errors
    ///
    /// Returns a typed service error when the input cannot be accepted.
    fn accept_input(&self, text: &str) -> Result<ServiceMessage, ServiceError>;
}

pub trait ActionInbox: Send + Sync {
    /// # Errors
    ///
    /// Returns a typed service error when the action cannot be queued.
    fn enqueue(&self, summary: &str) -> Result<ServiceMessage, ServiceError>;
}

pub trait TurnRunner: ProviderPort {
    /// # Errors
    ///
    /// Returns a typed service error when the turn cannot be prepared.
    fn prepare_turn(&self, profile_id: &ProfileId) -> Result<ServiceMessage, ServiceError>;
}

pub trait ContextBuilder: Send + Sync {
    /// # Errors
    ///
    /// Returns a typed service error when the context cannot be built.
    fn build_context(&self, sections: &[String]) -> Result<ServiceMessage, ServiceError>;
}

pub trait ToolCoordinator: ToolPort {
    /// # Errors
    ///
    /// Returns a typed service error when the tool is not authorized.
    fn authorize_tool(&self, name: &str) -> Result<ServiceMessage, ServiceError>;
}

pub trait CompactionManager: Send + Sync {
    /// # Errors
    ///
    /// Returns a typed service error when the compaction request is invalid.
    fn prepare_compaction(&self, target_tokens: usize) -> Result<ServiceMessage, ServiceError>;
}

pub trait GoalController: Send + Sync {
    /// # Errors
    ///
    /// Returns a typed service error when the goal is invalid.
    fn validate_goal(&self, objective: &str) -> Result<ServiceMessage, ServiceError>;
}

pub trait ChildManager: Send + Sync {
    /// # Errors
    ///
    /// Returns a typed service error when the child request is invalid.
    fn validate_child(&self, objective: &str) -> Result<ServiceMessage, ServiceError>;
}

pub trait KernelHandle: Send + Sync {
    /// # Errors
    ///
    /// Returns a typed service error when the kernel request is invalid.
    fn prepare_kernel(&self, language: &str) -> Result<ServiceMessage, ServiceError>;
}

pub trait ExtensionContext: Send + Sync {
    /// # Errors
    ///
    /// Returns a typed service error when the extension cannot be resolved.
    fn resolve_extension(&self, key: &str) -> Result<ServiceMessage, ServiceError>;
}

pub trait SessionWriter: RepositoryPort {}

#[derive(Clone)]
pub struct SessionServices {
    pub input_pipeline: Arc<dyn InputPipeline>,
    pub action_inbox: Arc<dyn ActionInbox>,
    pub turn_runner: Arc<dyn TurnRunner>,
    pub context_builder: Arc<dyn ContextBuilder>,
    pub tool_coordinator: Arc<dyn ToolCoordinator>,
    pub compaction_manager: Arc<dyn CompactionManager>,
    pub goal_controller: Arc<dyn GoalController>,
    pub child_manager: Arc<dyn ChildManager>,
    pub kernel_handle: Arc<dyn KernelHandle>,
    pub extension_context: Arc<dyn ExtensionContext>,
    pub session_writer: Arc<dyn SessionWriter>,
}

#[derive(Clone, Debug)]
pub struct LocalSessionServices {
    provider: String,
    model: String,
    allowed_tools: BTreeSet<String>,
    extensions: BTreeMap<String, String>,
    writer: JsonSessionWriter,
    max_input_bytes: usize,
    max_context_bytes: usize,
}

impl LocalSessionServices {
    pub fn new(
        writer: JsonSessionWriter,
        provider: impl Into<String>,
        model: impl Into<String>,
        allowed_tools: BTreeSet<String>,
        extensions: BTreeMap<String, String>,
    ) -> Self {
        Self {
            provider: provider.into(),
            model: model.into(),
            allowed_tools,
            extensions,
            writer,
            max_input_bytes: 1024 * 1024,
            max_context_bytes: 4 * 1024 * 1024,
        }
    }

    pub fn into_services(self) -> SessionServices {
        let service = Arc::new(self);
        SessionServices {
            input_pipeline: service.clone(),
            action_inbox: service.clone(),
            turn_runner: service.clone(),
            context_builder: service.clone(),
            tool_coordinator: service.clone(),
            compaction_manager: service.clone(),
            goal_controller: service.clone(),
            child_manager: service.clone(),
            kernel_handle: service.clone(),
            extension_context: service.clone(),
            session_writer: service,
        }
    }
}

impl ProviderPort for LocalSessionServices {
    fn provider_route(&self, _profile_id: &ProfileId) -> Result<(String, String), ServiceError> {
        if self.provider.trim().is_empty() || self.model.trim().is_empty() {
            Err(ServiceError::ProviderUnavailable)
        } else {
            Ok((self.provider.clone(), self.model.clone()))
        }
    }
}

impl ToolPort for LocalSessionServices {
    fn tool_allowed(&self, name: &str) -> bool {
        self.allowed_tools.contains(name)
    }
}

impl RepositoryPort for LocalSessionServices {
    fn load_snapshot(
        &self,
        session_id: &SessionId,
    ) -> Result<Option<AgentSessionSnapshot>, ServiceError> {
        self.writer.load_snapshot(session_id)
    }

    fn store_snapshot(&self, snapshot: &AgentSessionSnapshot) -> Result<(), ServiceError> {
        self.writer.store_snapshot(snapshot)
    }
}

impl ChannelPort for LocalSessionServices {
    fn normalize_input(&self, input: &str) -> Result<String, ServiceError> {
        let normalized = input.trim().replace("\r\n", "\n");
        if normalized.is_empty() || normalized.len() > self.max_input_bytes {
            Err(ServiceError::InvalidInput(
                "input must be non-empty and within the configured byte limit".into(),
            ))
        } else {
            Ok(normalized)
        }
    }
}

impl ClientPort for LocalSessionServices {
    fn validate_client_text(&self, input: &str) -> Result<(), ServiceError> {
        if input.contains('\0') {
            Err(ServiceError::InvalidInput(
                "client text cannot contain NUL".into(),
            ))
        } else {
            Ok(())
        }
    }
}

impl InputPipeline for LocalSessionServices {
    fn accept_input(&self, text: &str) -> Result<ServiceMessage, ServiceError> {
        self.validate_client_text(text)?;
        Ok(ServiceMessage::InputAccepted {
            normalized: self.normalize_input(text)?,
        })
    }
}

impl ActionInbox for LocalSessionServices {
    fn enqueue(&self, summary: &str) -> Result<ServiceMessage, ServiceError> {
        if summary.trim().is_empty() {
            Err(ServiceError::InvalidInput(
                "action summary cannot be empty".into(),
            ))
        } else {
            Ok(ServiceMessage::ActionQueued {
                action_id: ActionId::new(),
            })
        }
    }
}

impl TurnRunner for LocalSessionServices {
    fn prepare_turn(&self, profile_id: &ProfileId) -> Result<ServiceMessage, ServiceError> {
        let (provider, model) = self.provider_route(profile_id)?;
        Ok(ServiceMessage::TurnPrepared { provider, model })
    }
}

impl ContextBuilder for LocalSessionServices {
    fn build_context(&self, sections: &[String]) -> Result<ServiceMessage, ServiceError> {
        let content = sections.join("\n");
        if content.len() > self.max_context_bytes {
            Err(ServiceError::InvalidInput(
                "context exceeds the configured byte limit".into(),
            ))
        } else {
            Ok(ServiceMessage::ContextBuilt { content })
        }
    }
}

impl ToolCoordinator for LocalSessionServices {
    fn authorize_tool(&self, name: &str) -> Result<ServiceMessage, ServiceError> {
        if self.tool_allowed(name) {
            Ok(ServiceMessage::ToolAuthorized { name: name.into() })
        } else {
            Err(ServiceError::ToolUnauthorized(name.into()))
        }
    }
}

impl CompactionManager for LocalSessionServices {
    fn prepare_compaction(&self, target_tokens: usize) -> Result<ServiceMessage, ServiceError> {
        if target_tokens == 0 {
            Err(ServiceError::InvalidInput(
                "compaction target must be non-zero".into(),
            ))
        } else {
            Ok(ServiceMessage::CompactionPrepared { target_tokens })
        }
    }
}

impl GoalController for LocalSessionServices {
    fn validate_goal(&self, objective: &str) -> Result<ServiceMessage, ServiceError> {
        if objective.trim().is_empty() {
            Err(ServiceError::InvalidInput(
                "goal objective cannot be empty".into(),
            ))
        } else {
            Ok(ServiceMessage::GoalValidated {
                objective: objective.trim().into(),
            })
        }
    }
}

impl ChildManager for LocalSessionServices {
    fn validate_child(&self, objective: &str) -> Result<ServiceMessage, ServiceError> {
        if objective.trim().is_empty() {
            Err(ServiceError::InvalidInput(
                "child objective cannot be empty".into(),
            ))
        } else {
            Ok(ServiceMessage::ChildValidated {
                child_session_id: SessionId::new(),
            })
        }
    }
}

impl KernelHandle for LocalSessionServices {
    fn prepare_kernel(&self, language: &str) -> Result<ServiceMessage, ServiceError> {
        if language.trim().is_empty() {
            Err(ServiceError::InvalidInput(
                "kernel language cannot be empty".into(),
            ))
        } else {
            Ok(ServiceMessage::KernelPrepared {
                language: language.trim().into(),
            })
        }
    }
}

impl ExtensionContext for LocalSessionServices {
    fn resolve_extension(&self, key: &str) -> Result<ServiceMessage, ServiceError> {
        self.extensions
            .get(key)
            .cloned()
            .map(|value| ServiceMessage::ExtensionResolved {
                key: key.into(),
                value,
            })
            .ok_or_else(|| ServiceError::ExtensionMissing(key.into()))
    }
}

impl SessionWriter for LocalSessionServices {}

#[derive(Clone, Debug)]
pub struct JsonSessionWriter {
    directory: PathBuf,
}

impl JsonSessionWriter {
    pub fn new(directory: impl Into<PathBuf>) -> Self {
        Self {
            directory: directory.into(),
        }
    }

    fn path(&self, session_id: &SessionId) -> PathBuf {
        self.directory.join(format!("{session_id}.json"))
    }
}

impl RepositoryPort for JsonSessionWriter {
    fn load_snapshot(
        &self,
        session_id: &SessionId,
    ) -> Result<Option<AgentSessionSnapshot>, ServiceError> {
        let path = self.path(session_id);
        let bytes = match fs::read(path) {
            Ok(bytes) => bytes,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
            Err(error) => return Err(ServiceError::Persistence(error.to_string())),
        };
        serde_json::from_slice(&bytes)
            .map(Some)
            .map_err(|error| ServiceError::Persistence(error.to_string()))
    }

    fn store_snapshot(&self, snapshot: &AgentSessionSnapshot) -> Result<(), ServiceError> {
        fs::create_dir_all(&self.directory)
            .map_err(|error| ServiceError::Persistence(error.to_string()))?;
        let path = self.path(&snapshot.identity.session_id);
        let temporary = path.with_extension(format!("{}.tmp", std::process::id()));
        let bytes = keith_agent_types::canonical_json_bytes(snapshot)
            .map_err(|error| ServiceError::Persistence(error.to_string()))?;
        fs::write(&temporary, bytes)
            .map_err(|error| ServiceError::Persistence(error.to_string()))?;
        File::open(&temporary)
            .and_then(|file| file.sync_all())
            .map_err(|error| ServiceError::Persistence(error.to_string()))?;
        keith_platform::replace_file(&temporary, &path)
            .map_err(|error| ServiceError::Persistence(error.to_string()))?;
        File::open(&self.directory)
            .and_then(|file| file.sync_all())
            .map_err(|error| ServiceError::Persistence(error.to_string()))
    }
}

impl SessionWriter for JsonSessionWriter {}

#[derive(Clone, Debug, Eq, Error, PartialEq)]
pub enum SessionError {
    #[error("mailbox capacity must be non-zero")]
    InvalidMailboxCapacity,
    #[error("session mailbox is full")]
    MailboxFull,
    #[error("session actor is stopped")]
    ActorStopped,
    #[error("illegal session transition from {from:?} to {to:?}")]
    IllegalTransition {
        from: SessionRunState,
        to: SessionRunState,
    },
    #[error("session revision overflow")]
    RevisionOverflow,
    #[error("session clock failed")]
    Clock,
    #[error("session service failed: {0}")]
    Service(#[from] ServiceError),
    #[error("no durable session snapshot exists")]
    SnapshotMissing,
    #[error("session snapshot identity or schema is incompatible")]
    IncompatibleSnapshot,
}

struct ActorRequest {
    command: SessionCommand,
    response: SyncSender<Result<SessionReply, SessionError>>,
}

#[derive(Clone)]
pub struct AgentSessionHandle {
    sender: SyncSender<ActorRequest>,
    outstanding: Arc<AtomicUsize>,
    capacity: usize,
}

impl AgentSessionHandle {
    /// Submits one request to the serialized bounded actor mailbox.
    ///
    /// # Errors
    ///
    /// Returns a mailbox, lifecycle, transition, service, clock, or persistence error.
    pub fn dispatch(&self, command: SessionCommand) -> Result<SessionReply, SessionError> {
        self.outstanding
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |count| {
                (count < self.capacity).then_some(count + 1)
            })
            .map_err(|_| SessionError::MailboxFull)?;
        let reservation = OutstandingReservation(&self.outstanding);
        let (response, receive) = sync_channel(1);
        match self.sender.try_send(ActorRequest { command, response }) {
            Ok(()) => {}
            Err(TrySendError::Full(_)) => return Err(SessionError::MailboxFull),
            Err(TrySendError::Disconnected(_)) => return Err(SessionError::ActorStopped),
        }
        let result = receive.recv().map_err(|_| SessionError::ActorStopped)?;
        drop(reservation);
        result
    }

    /// # Errors
    ///
    /// Returns an actor or persistence error.
    pub fn snapshot(&self) -> Result<AgentSessionSnapshot, SessionError> {
        match self.dispatch(SessionCommand::Snapshot)? {
            SessionReply::Snapshot(snapshot) => Ok(snapshot),
            _ => Err(SessionError::ActorStopped),
        }
    }
}

struct OutstandingReservation<'a>(&'a AtomicUsize);

impl Drop for OutstandingReservation<'_> {
    fn drop(&mut self) {
        self.0.fetch_sub(1, Ordering::AcqRel);
    }
}

pub struct AgentSession;

impl AgentSession {
    /// Starts a root or durable child through the same actor constructor.
    ///
    /// # Errors
    ///
    /// Returns an error for an invalid capacity or initial persistence failure.
    pub fn spawn(
        identity: SessionIdentity,
        services: SessionServices,
        mailbox_capacity: usize,
    ) -> Result<AgentSessionHandle, SessionError> {
        let snapshot = AgentSessionSnapshot::initial(identity);
        services.session_writer.store_snapshot(&snapshot)?;
        Self::spawn_snapshot(snapshot, services, mailbox_capacity)
    }

    /// Reconstructs the actor from its durable writer snapshot.
    ///
    /// # Errors
    ///
    /// Returns an error for missing, corrupt, incompatible, or unstartable state.
    pub fn recover(
        identity: &SessionIdentity,
        services: SessionServices,
        mailbox_capacity: usize,
    ) -> Result<AgentSessionHandle, SessionError> {
        let snapshot = services
            .session_writer
            .load_snapshot(&identity.session_id)?
            .ok_or(SessionError::SnapshotMissing)?;
        if snapshot.version != CURRENT_SCHEMA_VERSION || snapshot.identity != *identity {
            return Err(SessionError::IncompatibleSnapshot);
        }
        Self::spawn_snapshot(snapshot, services, mailbox_capacity)
    }

    fn spawn_snapshot(
        snapshot: AgentSessionSnapshot,
        services: SessionServices,
        mailbox_capacity: usize,
    ) -> Result<AgentSessionHandle, SessionError> {
        if mailbox_capacity == 0 {
            return Err(SessionError::InvalidMailboxCapacity);
        }
        let (sender, receiver) = sync_channel(mailbox_capacity);
        thread::Builder::new()
            .name(format!("agent-session-{}", snapshot.identity.session_id))
            .spawn(move || run_actor(&receiver, snapshot, &services))
            .map_err(|_| SessionError::ActorStopped)?;
        Ok(AgentSessionHandle {
            sender,
            outstanding: Arc::new(AtomicUsize::new(0)),
            capacity: mailbox_capacity,
        })
    }
}

fn run_actor(
    receiver: &Receiver<ActorRequest>,
    mut snapshot: AgentSessionSnapshot,
    services: &SessionServices,
) {
    while let Ok(request) = receiver.recv() {
        let shutdown = request.command == SessionCommand::Shutdown;
        let result = apply_command(&mut snapshot, services, request.command);
        let _ = request.response.send(result);
        if shutdown {
            break;
        }
    }
}

fn apply_command(
    snapshot: &mut AgentSessionSnapshot,
    services: &SessionServices,
    command: SessionCommand,
) -> Result<SessionReply, SessionError> {
    match command {
        SessionCommand::Transition(next) => {
            transition(snapshot, next, services.session_writer.as_ref())?;
            Ok(SessionReply::Transitioned(snapshot.clone()))
        }
        SessionCommand::Invoke(request) => {
            let message = invoke_service(snapshot, services, request)?;
            apply_service_message(snapshot, message.clone(), services.session_writer.as_ref())?;
            Ok(SessionReply::ServiceApplied {
                message,
                snapshot: snapshot.clone(),
            })
        }
        SessionCommand::Snapshot => Ok(SessionReply::Snapshot(snapshot.clone())),
        SessionCommand::Shutdown => {
            services.session_writer.store_snapshot(snapshot)?;
            Ok(SessionReply::Shutdown(snapshot.clone()))
        }
    }
}

fn invoke_service(
    snapshot: &AgentSessionSnapshot,
    services: &SessionServices,
    request: ServiceRequest,
) -> Result<ServiceMessage, SessionError> {
    let message = match request {
        ServiceRequest::Input { text } => services.input_pipeline.accept_input(&text)?,
        ServiceRequest::EnqueueAction { summary } => services.action_inbox.enqueue(&summary)?,
        ServiceRequest::PrepareTurn => services
            .turn_runner
            .prepare_turn(&snapshot.identity.profile_id)?,
        ServiceRequest::BuildContext { sections } => {
            services.context_builder.build_context(&sections)?
        }
        ServiceRequest::AuthorizeTool { name } => {
            services.tool_coordinator.authorize_tool(&name)?
        }
        ServiceRequest::PrepareCompaction { target_tokens } => services
            .compaction_manager
            .prepare_compaction(target_tokens)?,
        ServiceRequest::ValidateGoal { objective } => {
            services.goal_controller.validate_goal(&objective)?
        }
        ServiceRequest::ValidateChild { objective } => {
            services.child_manager.validate_child(&objective)?
        }
        ServiceRequest::PrepareKernel { language } => {
            services.kernel_handle.prepare_kernel(&language)?
        }
        ServiceRequest::ResolveExtension { key } => {
            services.extension_context.resolve_extension(&key)?
        }
    };
    Ok(message)
}

fn apply_service_message(
    snapshot: &mut AgentSessionSnapshot,
    message: ServiceMessage,
    writer: &dyn SessionWriter,
) -> Result<(), SessionError> {
    let target = match &message {
        ServiceMessage::TurnPrepared { .. } => Some(SessionRunState::Running),
        ServiceMessage::ToolAuthorized { .. } => Some(SessionRunState::WaitingTool),
        ServiceMessage::CompactionPrepared { .. } => Some(SessionRunState::Compacting),
        ServiceMessage::ChildValidated { .. } => Some(SessionRunState::WaitingChild),
        ServiceMessage::KernelPrepared { .. } => Some(SessionRunState::WaitingExternal),
        _ => None,
    };
    let mut candidate = snapshot.clone();
    if let Some(target) = target {
        ensure_transition(candidate.state, target)?;
        candidate.state = target;
    }
    candidate.last_service_message = Some(message);
    advance(&mut candidate)?;
    writer.store_snapshot(&candidate)?;
    *snapshot = candidate;
    Ok(())
}

fn transition(
    snapshot: &mut AgentSessionSnapshot,
    next: SessionRunState,
    writer: &dyn SessionWriter,
) -> Result<(), SessionError> {
    ensure_transition(snapshot.state, next)?;
    if snapshot.state == next {
        return Ok(());
    }
    let mut candidate = snapshot.clone();
    candidate.state = next;
    advance(&mut candidate)?;
    writer.store_snapshot(&candidate)?;
    *snapshot = candidate;
    Ok(())
}

fn ensure_transition(from: SessionRunState, to: SessionRunState) -> Result<(), SessionError> {
    if from.allows(to) {
        Ok(())
    } else {
        Err(SessionError::IllegalTransition { from, to })
    }
}

fn advance(snapshot: &mut AgentSessionSnapshot) -> Result<(), SessionError> {
    snapshot.revision = snapshot
        .revision
        .checked_next()
        .ok_or(SessionError::RevisionOverflow)?;
    snapshot.updated_at = UtcTimestamp::now().map_err(|_| SessionError::Clock)?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use std::path::Path;
    use std::sync::{Arc, Barrier};

    use proptest::prelude::*;

    use super::*;

    fn identity(kind: SessionKind) -> SessionIdentity {
        let root = RootTreeId::new();
        let profile = ProfileId::new();
        match kind {
            SessionKind::Root => SessionIdentity::root(SessionId::new(), root, profile),
            SessionKind::DurableChild => {
                SessionIdentity::durable_child(SessionId::new(), root, SessionId::new(), profile)
            }
        }
    }

    fn services(path: &Path) -> SessionServices {
        LocalSessionServices::new(
            JsonSessionWriter::new(path),
            "provider",
            "model",
            BTreeSet::from(["shell".into()]),
            BTreeMap::from([("project".into(), "keith".into())]),
        )
        .into_services()
    }

    #[test]
    fn root_and_child_use_one_actor_and_snapshot_schema() {
        let directory = tempfile::tempdir().unwrap();
        for kind in [SessionKind::Root, SessionKind::DurableChild] {
            let identity = identity(kind);
            let handle =
                AgentSession::spawn(identity.clone(), services(directory.path()), 8).unwrap();
            let snapshot = handle.snapshot().unwrap();
            assert_eq!(snapshot.version, CURRENT_SCHEMA_VERSION);
            assert_eq!(snapshot.identity, identity);
            assert_eq!(snapshot.identity.kind, kind);
            handle.dispatch(SessionCommand::Shutdown).unwrap();
        }
    }

    #[test]
    fn services_return_typed_messages_and_actor_alone_commits_state() {
        let directory = tempfile::tempdir().unwrap();
        let identity = identity(SessionKind::Root);
        let handle = AgentSession::spawn(identity, services(directory.path()), 8).unwrap();
        handle
            .dispatch(SessionCommand::Transition(SessionRunState::Ready))
            .unwrap();
        let result = handle
            .dispatch(SessionCommand::Invoke(ServiceRequest::PrepareTurn))
            .unwrap();
        let SessionReply::ServiceApplied { message, snapshot } = result else {
            panic!("typed service response required");
        };
        assert!(matches!(message, ServiceMessage::TurnPrepared { .. }));
        assert_eq!(snapshot.state, SessionRunState::Running);
        let result = handle
            .dispatch(SessionCommand::Invoke(ServiceRequest::AuthorizeTool {
                name: "shell".into(),
            }))
            .unwrap();
        let SessionReply::ServiceApplied { snapshot, .. } = result else {
            panic!("typed service response required");
        };
        assert_eq!(snapshot.state, SessionRunState::WaitingTool);
    }

    #[test]
    fn illegal_transitions_are_rejected_without_revision_change() {
        let directory = tempfile::tempdir().unwrap();
        let handle =
            AgentSession::spawn(identity(SessionKind::Root), services(directory.path()), 8)
                .unwrap();
        let before = handle.snapshot().unwrap();
        assert!(matches!(
            handle.dispatch(SessionCommand::Transition(SessionRunState::Running)),
            Err(SessionError::IllegalTransition {
                from: SessionRunState::Dormant,
                to: SessionRunState::Running
            })
        ));
        assert_eq!(handle.snapshot().unwrap(), before);
    }

    #[test]
    fn durable_restart_reconstructs_equivalent_actor_state() {
        let directory = tempfile::tempdir().unwrap();
        let identity = identity(SessionKind::DurableChild);
        let handle = AgentSession::spawn(identity.clone(), services(directory.path()), 8).unwrap();
        handle
            .dispatch(SessionCommand::Transition(SessionRunState::Ready))
            .unwrap();
        handle
            .dispatch(SessionCommand::Invoke(ServiceRequest::PrepareTurn))
            .unwrap();
        handle
            .dispatch(SessionCommand::Invoke(ServiceRequest::ValidateChild {
                objective: "research".into(),
            }))
            .unwrap();
        let before = handle.snapshot().unwrap();
        handle.dispatch(SessionCommand::Shutdown).unwrap();

        let recovered = AgentSession::recover(&identity, services(directory.path()), 8).unwrap();
        assert_eq!(recovered.snapshot().unwrap(), before);
        recovered.dispatch(SessionCommand::Shutdown).unwrap();
    }

    #[test]
    fn concurrent_callers_are_serialized_and_mailbox_is_bounded() {
        let directory = tempfile::tempdir().unwrap();
        let handle =
            AgentSession::spawn(identity(SessionKind::Root), services(directory.path()), 2)
                .unwrap();
        let barrier = Arc::new(Barrier::new(16));
        let threads: Vec<_> = (0..16)
            .map(|index| {
                let handle = handle.clone();
                let barrier = Arc::clone(&barrier);
                thread::spawn(move || {
                    barrier.wait();
                    handle.dispatch(SessionCommand::Invoke(ServiceRequest::Input {
                        text: format!("input {index}"),
                    }))
                })
            })
            .collect();
        let results: Vec<_> = threads
            .into_iter()
            .map(|thread| thread.join().unwrap())
            .collect();
        assert!(results.iter().any(Result::is_ok));
        assert!(
            results
                .iter()
                .any(|result| matches!(result, Err(SessionError::MailboxFull)))
        );
        let snapshot = handle.snapshot().unwrap();
        let successful = results.iter().filter(|result| result.is_ok()).count();
        assert_eq!(snapshot.revision.get(), u64::try_from(successful).unwrap());
    }

    proptest! {
        #[test]
        fn transition_guard_matches_the_complete_state_graph(from in 0_usize..10, to in 0_usize..10) {
            let from = SessionRunState::ALL[from];
            let to = SessionRunState::ALL[to];
            prop_assert_eq!(ensure_transition(from, to).is_ok(), from.allows(to));
        }
    }

    #[test]
    fn every_state_pair_is_classified_by_the_guard() {
        let mut legal = 0;
        let mut illegal = 0;
        for from in SessionRunState::ALL {
            for to in SessionRunState::ALL {
                if ensure_transition(from, to).is_ok() {
                    legal += 1;
                } else {
                    illegal += 1;
                }
            }
        }
        assert_eq!(legal + illegal, 100);
        assert!(legal > 10);
        assert!(illegal > 0);
    }
}
