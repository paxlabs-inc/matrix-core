#![forbid(unsafe_code)]

use std::collections::BTreeMap;
use std::fs;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::thread;
use std::time::{Duration, Instant};

use keith_agent_types::{EntityId, Generation, RootTreeId, SessionId, UtcTimestamp, WorkerId};
use keith_connection::{
    LocalStream, connect_local, set_local_read_timeout, set_local_write_timeout,
};
use keith_runtime_api::{RuntimeRequest, RuntimeResponse};
use keith_worker_runtime::{
    LeaseError, LeaseGrant, LeaseManager, PrivateMessage, PrivateProtocolError, PrivateTransport,
    WorkerRegistration, WorkerRunState, read_registration, registration_path,
};
#[cfg(unix)]
use nix::errno::Errno;
#[cfg(unix)]
use nix::sys::signal::{Signal, kill};
#[cfg(unix)]
use nix::unistd::Pid;
use thiserror::Error;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum WorkerHealth {
    Starting,
    Healthy,
    Unresponsive,
    Draining,
    Exited,
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct WorkerResourceState {
    pub resident_bytes: Option<u64>,
    pub virtual_bytes: Option<u64>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WorkerStatus {
    pub worker_id: WorkerId,
    pub root_tree_id: RootTreeId,
    pub generation: Generation,
    pub pid: u32,
    pub health: WorkerHealth,
    pub heartbeat_at: UtcTimestamp,
    pub idle_for: Duration,
    pub resources: WorkerResourceState,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum WorkerEvent {
    Exited {
        root_tree_id: RootTreeId,
        generation: Generation,
        success: Option<bool>,
    },
    Fatal {
        root_tree_id: RootTreeId,
        generation: Generation,
        reason: String,
    },
}

#[derive(Clone, Debug)]
pub struct SupervisorOptions {
    pub startup_timeout: Duration,
    pub drain_timeout: Duration,
    pub stale_heartbeat: Duration,
    pub heartbeat_interval: Duration,
    pub lease_duration: Duration,
}

impl Default for SupervisorOptions {
    fn default() -> Self {
        Self {
            startup_timeout: Duration::from_secs(5),
            drain_timeout: Duration::from_secs(2),
            stale_heartbeat: Duration::from_secs(2),
            heartbeat_interval: Duration::from_millis(100),
            lease_duration: Duration::from_secs(2),
        }
    }
}

struct ManagedWorker {
    registration: WorkerRegistration,
    grant: LeaseGrant,
    control: Option<PrivateTransport<LocalStream>>,
    child: Option<Child>,
    last_activity: Instant,
    draining: bool,
}

pub struct WorkerSupervisor {
    state_dir: PathBuf,
    control_directory: PathBuf,
    next_control_id: u64,
    lease_database: PathBuf,
    executable: PathBuf,
    runtime_config: Option<PathBuf>,
    options: SupervisorOptions,
    leases: LeaseManager,
    workers: BTreeMap<RootTreeId, ManagedWorker>,
}

#[derive(Debug, Error)]
pub enum SupervisorError {
    #[error("worker process I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("worker registration failed: {0}")]
    Registration(#[from] keith_worker_runtime::WorkerRuntimeError),
    #[error(transparent)]
    Lease(#[from] LeaseError),
    #[error(transparent)]
    Private(#[from] PrivateProtocolError),
    #[error("worker {0} is already active")]
    AlreadyActive(RootTreeId),
    #[error("worker {0} is not active")]
    NotActive(RootTreeId),
    #[error("worker {root_tree_id} failed to become ready before the deadline")]
    StartupTimeout { root_tree_id: RootTreeId },
    #[error("worker {root_tree_id} exited during startup with {status}")]
    StartupExit {
        root_tree_id: RootTreeId,
        status: std::process::ExitStatus,
    },
    #[error("worker process control failed: {0}")]
    ProcessControl(String),
    #[error("route for root {root_tree_id} generation {generation:?} is stale")]
    StaleRoute {
        root_tree_id: RootTreeId,
        generation: Generation,
    },
    #[error("lease duration must exceed two heartbeat intervals")]
    InvalidLeaseDuration,
    #[error("worker runtime request failed: {0}")]
    Runtime(String),
    #[error("worker returned a result for a different runtime request")]
    MismatchedRuntimeResponse,
}

impl WorkerSupervisor {
    /// Opens the durable lease authority and constructs an empty supervisor.
    ///
    /// # Errors
    ///
    /// Returns an error when the lease database cannot be opened or timing is unsafe.
    pub fn open(
        state_dir: impl Into<PathBuf>,
        executable: impl Into<PathBuf>,
        options: SupervisorOptions,
    ) -> Result<Self, SupervisorError> {
        Self::open_internal(state_dir.into(), executable.into(), options, None)
    }

    /// Opens a supervisor that passes a non-secret runtime configuration to every worker.
    ///
    /// # Errors
    ///
    /// Returns an error when the lease database cannot be opened or timing is unsafe.
    pub fn open_with_runtime_config(
        state_dir: impl Into<PathBuf>,
        executable: impl Into<PathBuf>,
        options: SupervisorOptions,
        runtime_config: impl Into<PathBuf>,
    ) -> Result<Self, SupervisorError> {
        Self::open_internal(
            state_dir.into(),
            executable.into(),
            options,
            Some(runtime_config.into()),
        )
    }

    fn open_internal(
        state_dir: PathBuf,
        executable: PathBuf,
        options: SupervisorOptions,
        runtime_config: Option<PathBuf>,
    ) -> Result<Self, SupervisorError> {
        if options.heartbeat_interval.is_zero()
            || options.lease_duration <= options.heartbeat_interval.saturating_mul(2)
        {
            return Err(SupervisorError::InvalidLeaseDuration);
        }
        fs::create_dir_all(&state_dir)?;
        let lease_database = state_dir.join("leases.sqlite");
        let leases = LeaseManager::open(&lease_database)?;
        let control_directory =
            std::env::temp_dir().join(format!("keith-agent-control-{}", WorkerId::new()));
        Ok(Self {
            state_dir,
            control_directory,
            next_control_id: 1,
            lease_database,
            executable,
            runtime_config,
            options,
            leases,
            workers: BTreeMap::new(),
        })
    }

    pub fn lease_database_path(&self) -> &Path {
        &self.lease_database
    }

    /// Adopts live workers only when their registration and durable lease agree.
    ///
    /// # Errors
    ///
    /// Returns an error when registrations, leases, or authenticated control cannot be read.
    pub fn adopt_existing(&mut self) -> Result<Vec<WorkerStatus>, SupervisorError> {
        let directory = self.state_dir.join("workers");
        let entries = match fs::read_dir(directory) {
            Ok(entries) => entries,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
            Err(error) => return Err(error.into()),
        };
        for entry in entries {
            let path = entry?.path();
            if path.extension().and_then(|value| value.to_str()) != Some("json") {
                continue;
            }
            let registration = read_registration(&path)?;
            if !matches!(
                registration.state,
                WorkerRunState::Starting | WorkerRunState::Ready | WorkerRunState::Draining
            ) || !process_is_alive(registration.pid)
            {
                continue;
            }
            let Some(grant) = self.leases.current(&registration.root_tree_id)? else {
                continue;
            };
            if grant.worker_id != registration.worker_id
                || grant.generation != registration.generation
            {
                continue;
            }
            let control = connect_control(&registration, &grant, self.options.startup_timeout)?;
            self.workers
                .entry(registration.root_tree_id.clone())
                .or_insert(ManagedWorker {
                    registration,
                    grant,
                    control: Some(control),
                    child: None,
                    last_activity: Instant::now(),
                    draining: false,
                });
        }
        Ok(self.statuses())
    }

    /// Claims a lease, starts a worker, and authenticates its ready message.
    ///
    /// # Errors
    ///
    /// Returns an error when a live worker exists or claim, process, or handshake fails.
    pub fn start(&mut self, root_tree_id: RootTreeId) -> Result<WorkerStatus, SupervisorError> {
        if self
            .workers
            .get(&root_tree_id)
            .is_some_and(|worker| process_is_alive(worker.registration.pid))
        {
            return Err(SupervisorError::AlreadyActive(root_tree_id));
        }
        self.workers.remove(&root_tree_id);
        let grant =
            self.leases
                .claim(&root_tree_id, WorkerId::new(), self.options.lease_duration)?;
        let control_socket = self
            .control_directory
            .join(format!("worker-{}.sock", self.next_control_id));
        self.next_control_id = self.next_control_id.saturating_add(1);
        let mut child = match self.spawn(&grant, &control_socket) {
            Ok(child) => child,
            Err(error) => {
                let _ = self.leases.release(&grant);
                return Err(error);
            }
        };
        let deadline = Instant::now() + self.options.startup_timeout;
        let path = registration_path(&self.state_dir, &root_tree_id);
        loop {
            if let Some(status) = child.try_wait()? {
                let _ = self.leases.release(&grant);
                return Err(SupervisorError::StartupExit {
                    root_tree_id,
                    status,
                });
            }
            if let Ok(registration) = read_registration(&path)
                && registration.pid == child.id()
                && registration.worker_id == grant.worker_id
                && registration.generation == grant.generation
                && registration.state == WorkerRunState::Ready
            {
                match connect_control(&registration, &grant, self.options.startup_timeout) {
                    Ok(control) => {
                        let worker = ManagedWorker {
                            registration,
                            grant,
                            control: Some(control),
                            child: Some(child),
                            last_activity: Instant::now(),
                            draining: false,
                        };
                        let status = status_for(&worker, self.options.stale_heartbeat);
                        self.workers.insert(root_tree_id, worker);
                        return Ok(status);
                    }
                    Err(error) if Instant::now() < deadline => {
                        let _ = error;
                    }
                    Err(error) => {
                        let _ = child.kill();
                        let _ = child.wait();
                        let _ = self.leases.release(&grant);
                        return Err(error);
                    }
                }
            }
            if Instant::now() >= deadline {
                let _ = child.kill();
                let _ = child.wait();
                let _ = self.leases.release(&grant);
                return Err(SupervisorError::StartupTimeout { root_tree_id });
            }
            thread::sleep(Duration::from_millis(10));
        }
    }

    fn spawn(&self, grant: &LeaseGrant, control_socket: &Path) -> Result<Child, SupervisorError> {
        let heartbeat_ms = self.options.heartbeat_interval.as_millis().max(1);
        let lease_ms = self.options.lease_duration.as_millis().max(1);
        let mut command = Command::new(&self.executable);
        command
            .arg("--state-dir")
            .arg(&self.state_dir)
            .arg("--lease-db")
            .arg(&self.lease_database)
            .arg("--control-socket")
            .arg(control_socket)
            .arg("--root-tree")
            .arg(grant.root_tree_id.to_string())
            .arg("--worker-id")
            .arg(grant.worker_id.to_string())
            .arg("--generation")
            .arg(grant.generation.get().to_string())
            .arg("--authentication")
            .arg(grant.authentication.to_string())
            .arg("--expires-at")
            .arg(grant.expires_at.unix_millis().to_string())
            .arg("--heartbeat-ms")
            .arg(heartbeat_ms.to_string())
            .arg("--lease-ms")
            .arg(lease_ms.to_string());
        if let Some(runtime_config) = &self.runtime_config {
            command.arg("--runtime-config").arg(runtime_config);
        }
        command
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .map_err(SupervisorError::from)
    }

    pub fn statuses(&self) -> Vec<WorkerStatus> {
        self.workers
            .values()
            .map(|worker| status_for(worker, self.options.stale_heartbeat))
            .collect()
    }

    pub fn status(&self, root_tree_id: &RootTreeId) -> Option<WorkerStatus> {
        self.workers
            .get(root_tree_id)
            .map(|worker| status_for(worker, self.options.stale_heartbeat))
    }

    pub fn mark_activity(&mut self, root_tree_id: &RootTreeId) -> bool {
        self.workers.get_mut(root_tree_id).is_some_and(|worker| {
            worker.last_activity = Instant::now();
            true
        })
    }

    /// Rejects stale-generation routing before a command reaches a worker.
    ///
    /// # Errors
    ///
    /// Returns an error unless both in-memory and durable ownership match.
    pub fn validate_route(
        &self,
        root_tree_id: &RootTreeId,
        generation: Generation,
    ) -> Result<(), SupervisorError> {
        let Some(worker) = self.workers.get(root_tree_id) else {
            return Err(SupervisorError::StaleRoute {
                root_tree_id: root_tree_id.clone(),
                generation,
            });
        };
        if worker.grant.generation != generation || self.leases.validate(&worker.grant).is_err() {
            return Err(SupervisorError::StaleRoute {
                root_tree_id: root_tree_id.clone(),
                generation,
            });
        }
        Ok(())
    }

    /// Executes one request inside the authenticated worker that owns `root_tree_id`.
    /// Heartbeats continue to refresh health while a provider or tool request is running.
    ///
    /// # Errors
    ///
    /// Returns an error for stale ownership, private transport loss, or response mismatch.
    pub fn execute(
        &mut self,
        root_tree_id: &RootTreeId,
        generation: Generation,
        request: RuntimeRequest,
    ) -> Result<RuntimeResponse, SupervisorError> {
        self.validate_route(root_tree_id, generation)?;
        let worker = self
            .workers
            .get_mut(root_tree_id)
            .ok_or_else(|| SupervisorError::NotActive(root_tree_id.clone()))?;
        worker.last_activity = Instant::now();
        if worker.control.is_none() {
            worker.control = Some(connect_control(
                &worker.registration,
                &worker.grant,
                self.options.startup_timeout,
            )?);
        }
        let request_id = EntityId::new();
        let control = worker
            .control
            .as_mut()
            .ok_or_else(|| SupervisorError::NotActive(root_tree_id.clone()))?;
        control.send(PrivateMessage::Execute {
            request_id: request_id.clone(),
            request: Box::new(request),
        })?;
        loop {
            match control.receive() {
                Ok(PrivateMessage::ExecutionResult {
                    request_id: response_id,
                    response,
                }) if response_id == request_id => return Ok(*response),
                Ok(PrivateMessage::ExecutionResult { .. }) => {
                    return Err(SupervisorError::MismatchedRuntimeResponse);
                }
                Ok(PrivateMessage::Heartbeat { at }) => {
                    worker.registration.heartbeat_at = at;
                }
                Ok(PrivateMessage::Idle { .. } | PrivateMessage::Ready { .. }) => {}
                Ok(PrivateMessage::Fatal { reason }) => {
                    return Err(SupervisorError::Runtime(reason));
                }
                Ok(
                    PrivateMessage::SupervisorHello
                    | PrivateMessage::Execute { .. }
                    | PrivateMessage::CancelActive { .. }
                    | PrivateMessage::CancellationResult { .. }
                    | PrivateMessage::Shutdown { .. }
                    | PrivateMessage::ShutdownAck,
                ) => return Err(PrivateProtocolError::StaleRoute.into()),
                Err(error) if error.is_retryable_io() => {
                    self.leases.validate(&worker.grant)?;
                    if !process_is_alive(worker.registration.pid) {
                        return Err(SupervisorError::NotActive(root_tree_id.clone()));
                    }
                }
                Err(error) => return Err(error.into()),
            }
        }
    }

    /// Refreshes registrations and private messages and isolates worker exits.
    ///
    /// # Errors
    ///
    /// Returns an error when an owned child cannot be queried.
    pub fn monitor(&mut self) -> Result<Vec<WorkerEvent>, SupervisorError> {
        let roots: Vec<_> = self.workers.keys().cloned().collect();
        let mut events = Vec::new();
        for root in roots {
            let mut exited = None;
            if let Some(worker) = self.workers.get_mut(&root) {
                exited = if let Some(child) = worker.child.as_mut() {
                    child.try_wait()?.map(|status| status.success())
                } else if process_is_alive(worker.registration.pid) {
                    None
                } else {
                    Some(false)
                };
                if exited.is_none() {
                    if let Some(control) = worker.control.as_mut() {
                        match control.receive() {
                            Ok(PrivateMessage::Heartbeat { at }) => {
                                worker.registration.heartbeat_at = at;
                            }
                            Ok(PrivateMessage::Fatal { reason }) => {
                                events.push(WorkerEvent::Fatal {
                                    root_tree_id: root.clone(),
                                    generation: worker.registration.generation,
                                    reason,
                                });
                            }
                            Ok(
                                PrivateMessage::Idle { since: _ }
                                | PrivateMessage::Ready { .. }
                                | PrivateMessage::Execute { .. }
                                | PrivateMessage::ExecutionResult { .. }
                                | PrivateMessage::CancelActive { .. }
                                | PrivateMessage::CancellationResult { .. }
                                | PrivateMessage::ShutdownAck
                                | PrivateMessage::SupervisorHello
                                | PrivateMessage::Shutdown { .. },
                            ) => {}
                            Err(error) if error.is_retryable_io() => {}
                            Err(error) if error.is_connection_loss() => worker.control = None,
                            Err(_) => worker.control = None,
                        }
                    }
                    let path = registration_path(&self.state_dir, &root);
                    if let Ok(registration) = read_registration(&path)
                        && registration.pid == worker.registration.pid
                        && registration.generation == worker.registration.generation
                    {
                        worker.registration = registration;
                    }
                }
            }
            if let Some(success) = exited
                && let Some(worker) = self.workers.remove(&root)
            {
                let _ = self.leases.release(&worker.grant);
                events.push(WorkerEvent::Exited {
                    root_tree_id: root,
                    generation: worker.registration.generation,
                    success: Some(success),
                });
            }
        }
        Ok(events)
    }

    /// Requests authenticated drain, then uses OS termination at the deadline.
    ///
    /// # Errors
    ///
    /// Returns an error when no worker exists or fallback signaling fails.
    pub fn drain(&mut self, root_tree_id: &RootTreeId) -> Result<(), SupervisorError> {
        let worker = self
            .workers
            .get_mut(root_tree_id)
            .ok_or_else(|| SupervisorError::NotActive(root_tree_id.clone()))?;
        worker.draining = true;
        let deadline_at = UtcTimestamp::now()
            .ok()
            .and_then(|now| add_duration(now, self.options.drain_timeout))
            .unwrap_or(UtcTimestamp::UNIX_EPOCH);
        let requested = worker.control.as_mut().is_some_and(|control| {
            control
                .send(PrivateMessage::Shutdown {
                    deadline: deadline_at,
                })
                .is_ok()
        });
        if !requested {
            signal(worker.registration.pid, ProcessSignal::Graceful)?;
        }
        let pid = worker.registration.pid;
        let deadline = Instant::now() + self.options.drain_timeout;
        while process_is_alive(pid) && Instant::now() < deadline {
            thread::sleep(Duration::from_millis(10));
        }
        if process_is_alive(pid) {
            signal(pid, ProcessSignal::Force)?;
        }
        if let Some(child) = worker.child.as_mut() {
            let _ = child.wait();
        } else {
            while process_is_alive(pid) {
                thread::sleep(Duration::from_millis(5));
            }
        }
        let worker = self
            .workers
            .remove(root_tree_id)
            .ok_or_else(|| SupervisorError::NotActive(root_tree_id.clone()))?;
        match self.leases.release(&worker.grant) {
            Ok(()) | Err(LeaseError::OwnershipLost(_)) => Ok(()),
            Err(error) => Err(error.into()),
        }
    }

    /// Immediately terminates and replaces a worker with a new generation.
    ///
    /// # Errors
    ///
    /// Returns an error when termination, release, or replacement startup fails.
    pub fn force_replace(
        &mut self,
        root_tree_id: &RootTreeId,
    ) -> Result<WorkerStatus, SupervisorError> {
        let mut worker = self
            .workers
            .remove(root_tree_id)
            .ok_or_else(|| SupervisorError::NotActive(root_tree_id.clone()))?;
        signal(worker.registration.pid, ProcessSignal::Force)?;
        if let Some(child) = worker.child.as_mut() {
            let _ = child.wait();
        } else {
            while process_is_alive(worker.registration.pid) {
                thread::sleep(Duration::from_millis(5));
            }
        }
        self.leases.release(&worker.grant)?;
        self.start(root_tree_id.clone())
    }

    /// Stops all workers as part of structured daemon shutdown.
    ///
    /// # Errors
    ///
    /// Returns the first worker shutdown error after attempting every worker.
    pub fn drain_all(&mut self) -> Result<(), SupervisorError> {
        let roots: Vec<_> = self.workers.keys().cloned().collect();
        let mut first_error = None;
        for root in roots {
            if let Err(error) = self.drain(&root)
                && first_error.is_none()
            {
                first_error = Some(error);
            }
        }
        first_error.map_or(Ok(()), Err)
    }

    /// Gracefully replaces a worker with the next transactionally assigned generation.
    ///
    /// # Errors
    ///
    /// Returns an error when drain or replacement startup fails.
    pub fn restart(&mut self, root_tree_id: &RootTreeId) -> Result<WorkerStatus, SupervisorError> {
        if self.workers.contains_key(root_tree_id) {
            self.drain(root_tree_id)?;
        }
        self.start(root_tree_id.clone())
    }

    /// Drains workers whose supervisor-observed activity exceeds `idle_limit`.
    ///
    /// # Errors
    ///
    /// Returns an error when an idle worker cannot be stopped.
    pub fn evict_idle(&mut self, idle_limit: Duration) -> Result<Vec<RootTreeId>, SupervisorError> {
        let roots: Vec<_> = self
            .workers
            .iter()
            .filter(|(_, worker)| worker.last_activity.elapsed() >= idle_limit)
            .map(|(root, _)| root.clone())
            .collect();
        for root in &roots {
            self.drain(root)?;
        }
        Ok(roots)
    }
}

/// Signals the active request owned by a durable worker lease without borrowing its supervisor.
///
/// This auxiliary route allows cancellation to reach the worker while the primary supervisor
/// connection is synchronously waiting for the active runtime request.
///
/// # Errors
///
/// Returns an error when the registration and lease disagree or authenticated delivery fails.
pub fn signal_active_cancellation(
    state_dir: &Path,
    root_tree_id: &RootTreeId,
    session_id: &SessionId,
    timeout: Duration,
) -> Result<bool, SupervisorError> {
    let registration = read_registration(&registration_path(state_dir, root_tree_id))?;
    let leases = LeaseManager::open(&state_dir.join("leases.sqlite"))?;
    let grant = leases
        .current(root_tree_id)?
        .ok_or_else(|| SupervisorError::NotActive(root_tree_id.clone()))?;
    if registration.root_tree_id != *root_tree_id
        || registration.worker_id != grant.worker_id
        || registration.generation != grant.generation
        || !matches!(
            registration.state,
            WorkerRunState::Starting | WorkerRunState::Ready | WorkerRunState::Draining
        )
        || !process_is_alive(registration.pid)
    {
        return Err(SupervisorError::StaleRoute {
            root_tree_id: root_tree_id.clone(),
            generation: grant.generation,
        });
    }
    leases.validate(&grant)?;
    let deadline = Instant::now() + timeout;
    'attempt: loop {
        let remaining = deadline.saturating_duration_since(Instant::now());
        if remaining.is_zero() {
            return Ok(false);
        }
        leases.validate(&grant)?;
        let stream = match connect_local(&registration.control_socket) {
            Ok(stream) => stream,
            Err(keith_connection::ConnectionError::Io(error))
                if matches!(
                    error.kind(),
                    std::io::ErrorKind::NotFound | std::io::ErrorKind::ConnectionRefused
                ) && Instant::now() < deadline =>
            {
                thread::sleep(Duration::from_millis(5));
                continue 'attempt;
            }
            Err(error) => {
                return Err(SupervisorError::ProcessControl(format!(
                    "local worker connection failed: {error}"
                )));
            }
        };
        set_local_read_timeout(&stream, Some(remaining))?;
        set_local_write_timeout(&stream, Some(remaining))?;
        let mut control = PrivateTransport::new(stream, grant.clone())?;
        let request_id = EntityId::new();
        control.send(PrivateMessage::CancelActive {
            request_id: request_id.clone(),
            session_id: session_id.clone(),
        })?;
        loop {
            match control.receive() {
                Ok(PrivateMessage::CancellationResult {
                    request_id: response_id,
                    result,
                }) if response_id == request_id => match result {
                    Ok(true) => return Ok(true),
                    Ok(false) if Instant::now() < deadline => {
                        thread::sleep(Duration::from_millis(5));
                        continue 'attempt;
                    }
                    Ok(false) => return Ok(false),
                    Err(error) => return Err(SupervisorError::Runtime(error)),
                },
                Ok(PrivateMessage::Heartbeat { .. } | PrivateMessage::Idle { .. }) => {}
                Ok(_) => return Err(PrivateProtocolError::StaleRoute.into()),
                Err(error) if error.is_retryable_io() && Instant::now() < deadline => {}
                Err(error) => return Err(error.into()),
            }
            if Instant::now() >= deadline {
                return Ok(false);
            }
        }
    }
}

fn connect_control(
    registration: &WorkerRegistration,
    grant: &LeaseGrant,
    timeout: Duration,
) -> Result<PrivateTransport<LocalStream>, SupervisorError> {
    let deadline = Instant::now() + timeout;
    'connect: loop {
        match connect_local(&registration.control_socket) {
            Ok(stream) => {
                set_local_read_timeout(&stream, Some(Duration::from_millis(20)))?;
                set_local_write_timeout(&stream, Some(Duration::from_secs(1)))?;
                let mut control = PrivateTransport::new(stream, grant.clone())?;
                if let Err(error) = control.send(PrivateMessage::SupervisorHello) {
                    if error.is_connection_loss() && Instant::now() < deadline {
                        thread::sleep(Duration::from_millis(10));
                        continue 'connect;
                    }
                    return Err(error.into());
                }
                loop {
                    match control.receive() {
                        Ok(PrivateMessage::Ready { pid }) if pid == registration.pid => {
                            return Ok(control);
                        }
                        Ok(PrivateMessage::Heartbeat { .. } | PrivateMessage::Idle { .. }) => {}
                        Ok(_) => return Err(PrivateProtocolError::StaleRoute.into()),
                        Err(error) if error.is_retryable_io() && Instant::now() < deadline => {}
                        Err(error) if error.is_connection_loss() && Instant::now() < deadline => {
                            thread::sleep(Duration::from_millis(10));
                            continue 'connect;
                        }
                        Err(error) => return Err(error.into()),
                    }
                    if Instant::now() >= deadline {
                        return Err(SupervisorError::StartupTimeout {
                            root_tree_id: registration.root_tree_id.clone(),
                        });
                    }
                }
            }
            Err(keith_connection::ConnectionError::Io(error))
                if matches!(
                    error.kind(),
                    std::io::ErrorKind::NotFound | std::io::ErrorKind::ConnectionRefused
                ) && Instant::now() < deadline =>
            {
                thread::sleep(Duration::from_millis(10));
            }
            Err(error) => {
                return Err(SupervisorError::ProcessControl(format!(
                    "local worker connection failed: {error}"
                )));
            }
        }
    }
}

#[derive(Clone, Copy, Eq, PartialEq)]
enum ProcessSignal {
    Graceful,
    Force,
}

#[cfg(unix)]
fn signal(pid: u32, signal: ProcessSignal) -> Result<(), SupervisorError> {
    let raw_pid = i32::try_from(pid)
        .map_err(|_| SupervisorError::ProcessControl("process ID is out of range".into()))?;
    let signal = match signal {
        ProcessSignal::Graceful => Signal::SIGTERM,
        ProcessSignal::Force => Signal::SIGKILL,
    };
    kill(Pid::from_raw(raw_pid), signal)
        .map_err(|error| SupervisorError::ProcessControl(error.to_string()))
}

#[cfg(windows)]
fn signal(pid: u32, signal: ProcessSignal) -> Result<(), SupervisorError> {
    let mut command = Command::new("taskkill");
    command.args(["/PID", &pid.to_string(), "/T"]);
    if signal == ProcessSignal::Force {
        command.arg("/F");
    }
    let status = command
        .status()
        .map_err(|error| SupervisorError::ProcessControl(error.to_string()))?;
    if status.success() || !process_is_alive(pid) {
        Ok(())
    } else {
        Err(SupervisorError::ProcessControl(format!(
            "taskkill exited with {status}"
        )))
    }
}

#[cfg(not(any(unix, windows)))]
fn signal(_pid: u32, _signal: ProcessSignal) -> Result<(), SupervisorError> {
    Err(SupervisorError::ProcessControl(
        "this platform has no process-control backend".into(),
    ))
}

#[cfg(unix)]
fn process_is_alive(pid: u32) -> bool {
    let Ok(raw_pid) = i32::try_from(pid) else {
        return false;
    };
    if process_is_zombie(pid) {
        return false;
    }
    match kill(Pid::from_raw(raw_pid), None) {
        Ok(()) | Err(Errno::EPERM) => true,
        Err(_) => false,
    }
}

#[cfg(windows)]
fn process_is_alive(pid: u32) -> bool {
    let Ok(output) = Command::new("tasklist")
        .args(["/FI", &format!("PID eq {pid}"), "/FO", "CSV", "/NH"])
        .output()
    else {
        return false;
    };
    String::from_utf8_lossy(&output.stdout).lines().any(|line| {
        line.split(',')
            .nth(1)
            .is_some_and(|value| value.trim_matches('"') == pid.to_string())
    })
}

#[cfg(not(any(unix, windows)))]
fn process_is_alive(_pid: u32) -> bool {
    false
}

#[cfg(target_os = "linux")]
fn process_is_zombie(pid: u32) -> bool {
    fs::read_to_string(Path::new("/proc").join(pid.to_string()).join("stat"))
        .ok()
        .and_then(|stat| {
            stat.rsplit_once(") ")
                .map(|(_, fields)| fields.starts_with('Z'))
        })
        .unwrap_or(false)
}

#[cfg(all(unix, not(target_os = "linux")))]
fn process_is_zombie(_pid: u32) -> bool {
    false
}

fn status_for(worker: &ManagedWorker, stale_heartbeat: Duration) -> WorkerStatus {
    let heartbeat_age = UtcTimestamp::now()
        .ok()
        .and_then(|now| {
            now.unix_millis()
                .checked_sub(worker.registration.heartbeat_at.unix_millis())
        })
        .and_then(|millis| u64::try_from(millis).ok())
        .map_or(Duration::ZERO, Duration::from_millis);
    let health = if !process_is_alive(worker.registration.pid) {
        WorkerHealth::Exited
    } else if worker.draining || worker.registration.state == WorkerRunState::Draining {
        WorkerHealth::Draining
    } else if worker.registration.state == WorkerRunState::Starting {
        WorkerHealth::Starting
    } else if heartbeat_age > stale_heartbeat {
        WorkerHealth::Unresponsive
    } else {
        WorkerHealth::Healthy
    };
    WorkerStatus {
        worker_id: worker.registration.worker_id.clone(),
        root_tree_id: worker.registration.root_tree_id.clone(),
        generation: worker.registration.generation,
        pid: worker.registration.pid,
        health,
        heartbeat_at: worker.registration.heartbeat_at,
        idle_for: worker.last_activity.elapsed(),
        resources: read_resources(worker.registration.pid),
    }
}

#[cfg(target_os = "linux")]
fn read_resources(pid: u32) -> WorkerResourceState {
    let Ok(status) = fs::read_to_string(Path::new("/proc").join(pid.to_string()).join("status"))
    else {
        return WorkerResourceState::default();
    };
    let kibibytes = |prefix: &str| {
        status
            .lines()
            .find_map(|line| line.strip_prefix(prefix))
            .and_then(|value| value.split_whitespace().next())
            .and_then(|value| value.parse::<u64>().ok())
            .and_then(|value| value.checked_mul(1024))
    };
    WorkerResourceState {
        resident_bytes: kibibytes("VmRSS:"),
        virtual_bytes: kibibytes("VmSize:"),
    }
}

#[cfg(target_os = "macos")]
fn read_resources(pid: u32) -> WorkerResourceState {
    let Ok(output) = Command::new("ps")
        .args(["-o", "rss=", "-o", "vsz=", "-p", &pid.to_string()])
        .output()
    else {
        return WorkerResourceState::default();
    };
    let mut values = String::from_utf8_lossy(&output.stdout)
        .split_whitespace()
        .filter_map(|value| value.parse::<u64>().ok())
        .map(|value| value.saturating_mul(1024));
    WorkerResourceState {
        resident_bytes: values.next(),
        virtual_bytes: values.next(),
    }
}

#[cfg(windows)]
fn read_resources(pid: u32) -> WorkerResourceState {
    let Ok(output) = Command::new("tasklist")
        .args(["/FI", &format!("PID eq {pid}"), "/FO", "CSV", "/NH"])
        .output()
    else {
        return WorkerResourceState::default();
    };
    let resident_bytes = String::from_utf8_lossy(&output.stdout)
        .lines()
        .next()
        .and_then(|line| line.rsplit_once(',').map(|(_, memory)| memory))
        .map(|memory| {
            memory
                .chars()
                .filter(char::is_ascii_digit)
                .collect::<String>()
        })
        .and_then(|memory| memory.parse::<u64>().ok())
        .map(|value| value.saturating_mul(1024));
    WorkerResourceState {
        resident_bytes,
        virtual_bytes: None,
    }
}

#[cfg(not(any(target_os = "linux", target_os = "macos", windows)))]
fn read_resources(_pid: u32) -> WorkerResourceState {
    WorkerResourceState::default()
}

fn add_duration(timestamp: UtcTimestamp, duration: Duration) -> Option<UtcTimestamp> {
    i64::try_from(duration.as_millis())
        .ok()
        .and_then(|millis| timestamp.unix_millis().checked_add(millis))
        .map(UtcTimestamp::from_unix_millis)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn missing_registration_directory_is_an_empty_adoption_set() {
        let directory = tempfile::tempdir().unwrap();
        let mut supervisor = WorkerSupervisor::open(
            directory.path(),
            "/not/started/by-this-test",
            SupervisorOptions::default(),
        )
        .unwrap();
        assert!(supervisor.adopt_existing().unwrap().is_empty());
    }
}
