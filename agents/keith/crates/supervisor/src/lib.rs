#![forbid(unsafe_code)]

mod image;

pub use image::{
    ImageInstallRequest, ImageRegistryError, InstalledImage, WorkerImageDataInventory,
    WorkerImageRegistry, worker_image_data_inventory,
};

use std::collections::{BTreeMap, VecDeque};
use std::fs;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::thread;
use std::time::{Duration, Instant};

use keith_agent_types::{
    EntityId, Generation, ProfileId, RootTreeId, SessionId, UtcTimestamp, WorkerId,
};
use keith_connection::{
    LocalStream, connect_local, set_local_read_timeout, set_local_write_timeout,
};
use keith_runtime_api::{RuntimeEvent, RuntimeRequest, RuntimeResponse};
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
use sha2::{Digest, Sha256};
use thiserror::Error;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum WorkerHealth {
    Starting,
    Healthy,
    Unresponsive,
    Draining,
    Exited,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AuxiliaryProcessKind {
    Browser,
    Display,
    StreamBridge,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AuxiliaryProcessHealth {
    Running,
    Exited,
    Quarantined,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AuxiliaryProcessSpec {
    pub id: EntityId,
    pub profile_id: ProfileId,
    pub kind: AuxiliaryProcessKind,
    pub executable: PathBuf,
    pub arguments: Vec<String>,
    pub runtime_directory: PathBuf,
    pub environment: BTreeMap<String, String>,
    pub crash_limit: usize,
    pub crash_window: Duration,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AuxiliaryProcessStatus {
    pub id: EntityId,
    pub profile_id: ProfileId,
    pub kind: AuxiliaryProcessKind,
    pub pid: Option<u32>,
    pub health: AuxiliaryProcessHealth,
    pub crashes_in_window: usize,
}

struct ManagedAuxiliaryProcess {
    spec: AuxiliaryProcessSpec,
    child: Option<Child>,
    crashes: VecDeque<Instant>,
    quarantined: bool,
}

#[derive(Default)]
pub struct AuxiliaryProcessSupervisor {
    processes: BTreeMap<EntityId, ManagedAuxiliaryProcess>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct HeadedBrowserStatus {
    pub profile_id: ProfileId,
    pub display: AuxiliaryProcessStatus,
    pub browser: AuxiliaryProcessStatus,
}

#[derive(Clone, Debug, Eq, PartialEq)]
struct HeadedBrowserProcesses {
    display_id: EntityId,
    browser_id: EntityId,
}

#[derive(Default)]
pub struct HeadedBrowserSupervisor {
    auxiliary: AuxiliaryProcessSupervisor,
    profiles: BTreeMap<ProfileId, HeadedBrowserProcesses>,
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
    pub image_id: String,
    pub image_manifest_sha256: String,
    pub source_manifest_sha256: String,
    pub pid: u32,
    pub health: WorkerHealth,
    pub heartbeat_at: UtcTimestamp,
    pub idle_for: Duration,
    pub resources: WorkerResourceState,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WorkerRollProof {
    pub root_tree_id: RootTreeId,
    pub previous_generation: Generation,
    pub previous_image_id: String,
    pub generation: Generation,
    pub image_id: String,
    pub health: WorkerHealth,
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

#[derive(Clone)]
struct AdoptedCleanupCandidate {
    registration: WorkerRegistration,
    process_start_identity: Option<String>,
    executable: PathBuf,
    executable_sha256: String,
}

pub struct WorkerSupervisor {
    state_dir: PathBuf,
    control_directory: PathBuf,
    next_control_id: u64,
    lease_database: PathBuf,
    images: WorkerImageRegistry,
    pinned_images: BTreeMap<String, InstalledImage>,
    canary_mode: bool,
    runtime_config: Option<PathBuf>,
    options: SupervisorOptions,
    leases: LeaseManager,
    workers: BTreeMap<RootTreeId, ManagedWorker>,
    adopted_cleanup: BTreeMap<RootTreeId, AdoptedCleanupCandidate>,
}

#[derive(Debug, Error)]
pub enum SupervisorError {
    #[error("worker process I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error(transparent)]
    Image(#[from] ImageRegistryError),
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
    #[error("auxiliary process {0} is already active")]
    AuxiliaryAlreadyActive(EntityId),
    #[error("auxiliary process {0} is not registered")]
    AuxiliaryNotActive(EntityId),
    #[error("auxiliary process {0} is quarantined after repeated crashes")]
    AuxiliaryQuarantined(EntityId),
    #[error("auxiliary process specification is invalid: {0}")]
    InvalidAuxiliary(String),
    #[error(
        "worker {root_tree_id} failed to roll to image {candidate_image_id}: {roll_error}; rollback to {previous_image_id} failed: {rollback_error}"
    )]
    RollbackFailed {
        root_tree_id: RootTreeId,
        candidate_image_id: String,
        previous_image_id: String,
        roll_error: String,
        rollback_error: String,
    },
}

impl AuxiliaryProcessSupervisor {
    /// Starts an installation-owned browser, display, or stream process with a cleared environment.
    ///
    /// # Errors
    /// Returns an error for invalid process boundaries, duplicate identities, quarantine, or spawn
    /// failure.
    pub fn start(
        &mut self,
        spec: AuxiliaryProcessSpec,
    ) -> Result<AuxiliaryProcessStatus, SupervisorError> {
        validate_auxiliary_spec(&spec)?;
        if let Some(existing) = self.processes.get(&spec.id) {
            if existing.quarantined {
                return Err(SupervisorError::AuxiliaryQuarantined(spec.id));
            }
            if existing.child.is_some() {
                return Err(SupervisorError::AuxiliaryAlreadyActive(spec.id));
            }
        }
        fs::create_dir_all(&spec.runtime_directory)?;
        let child = spawn_auxiliary(&spec)?;
        let id = spec.id.clone();
        let process = self
            .processes
            .entry(id.clone())
            .or_insert_with(|| ManagedAuxiliaryProcess {
                spec: spec.clone(),
                child: None,
                crashes: VecDeque::new(),
                quarantined: false,
            });
        process.spec = spec;
        process.child = Some(child);
        Ok(auxiliary_status(process))
    }

    /// Observes process exits and advances bounded crash-loop quarantine state.
    ///
    /// # Errors
    /// Returns an error when the process is unknown or its status cannot be observed.
    pub fn poll(&mut self, id: &EntityId) -> Result<AuxiliaryProcessStatus, SupervisorError> {
        let process = self
            .processes
            .get_mut(id)
            .ok_or_else(|| SupervisorError::AuxiliaryNotActive(id.clone()))?;
        let exited = process
            .child
            .as_mut()
            .map(|child| child.try_wait())
            .transpose()?
            .flatten()
            .is_some();
        if exited {
            process.child = None;
            let now = Instant::now();
            process.crashes.push_back(now);
            while process
                .crashes
                .front()
                .is_some_and(|crash| now.duration_since(*crash) > process.spec.crash_window)
            {
                process.crashes.pop_front();
            }
            process.quarantined = process.crashes.len() >= process.spec.crash_limit;
        }
        Ok(auxiliary_status(process))
    }

    /// Restarts a known exited process unless its durable owner has quarantined it.
    ///
    /// # Errors
    /// Returns an error for unknown, running, quarantined, invalid, or unspawnable processes.
    pub fn restart(&mut self, id: &EntityId) -> Result<AuxiliaryProcessStatus, SupervisorError> {
        let process = self
            .processes
            .get_mut(id)
            .ok_or_else(|| SupervisorError::AuxiliaryNotActive(id.clone()))?;
        if process.quarantined {
            return Err(SupervisorError::AuxiliaryQuarantined(id.clone()));
        }
        if process.child.is_some() {
            return Err(SupervisorError::AuxiliaryAlreadyActive(id.clone()));
        }
        validate_auxiliary_spec(&process.spec)?;
        process.child = Some(spawn_auxiliary(&process.spec)?);
        Ok(auxiliary_status(process))
    }

    /// Terminates and forgets one auxiliary process.
    ///
    /// # Errors
    /// Returns an error when the identity is unknown or process control fails.
    pub fn stop(&mut self, id: &EntityId) -> Result<AuxiliaryProcessStatus, SupervisorError> {
        let mut process = self
            .processes
            .remove(id)
            .ok_or_else(|| SupervisorError::AuxiliaryNotActive(id.clone()))?;
        if let Some(mut child) = process.child.take() {
            child.kill()?;
            child.wait()?;
        }
        Ok(AuxiliaryProcessStatus {
            id: process.spec.id,
            profile_id: process.spec.profile_id,
            kind: process.spec.kind,
            pid: None,
            health: AuxiliaryProcessHealth::Exited,
            crashes_in_window: process.crashes.len(),
        })
    }

    pub fn status(&self, id: &EntityId) -> Option<AuxiliaryProcessStatus> {
        self.processes.get(id).map(auxiliary_status)
    }
}

impl HeadedBrowserSupervisor {
    /// Starts one display and one headed browser process for a profile as a single supervised
    /// allocation. A browser spawn failure tears down the display before returning.
    ///
    /// # Errors
    /// Returns an error when the pair is malformed, the profile is active, or either process
    /// cannot be started.
    pub fn start(
        &mut self,
        display: AuxiliaryProcessSpec,
        browser: AuxiliaryProcessSpec,
    ) -> Result<HeadedBrowserStatus, SupervisorError> {
        if display.kind != AuxiliaryProcessKind::Display
            || browser.kind != AuxiliaryProcessKind::Browser
            || display.profile_id != browser.profile_id
            || display.id == browser.id
        {
            return Err(SupervisorError::InvalidAuxiliary(
                "headed browser requires distinct display and browser identities for one profile"
                    .into(),
            ));
        }
        let profile_id = display.profile_id.clone();
        if self.profiles.contains_key(&profile_id) {
            return Err(SupervisorError::InvalidAuxiliary(format!(
                "headed browser is already registered for profile {profile_id}"
            )));
        }
        let display_status = self.auxiliary.start(display)?;
        let display_id = display_status.id.clone();
        let browser_status = match self.auxiliary.start(browser) {
            Ok(status) => status,
            Err(error) => {
                let _ = self.auxiliary.stop(&display_id);
                return Err(error);
            }
        };
        self.profiles.insert(
            profile_id.clone(),
            HeadedBrowserProcesses {
                display_id,
                browser_id: browser_status.id.clone(),
            },
        );
        Ok(HeadedBrowserStatus {
            profile_id,
            display: display_status,
            browser: browser_status,
        })
    }

    /// Observes both processes and returns quarantine state without attempting an unbounded
    /// automatic restart.
    ///
    /// # Errors
    /// Returns an error when the profile is unknown or process status cannot be observed.
    pub fn poll(&mut self, profile_id: &ProfileId) -> Result<HeadedBrowserStatus, SupervisorError> {
        let pair = self.profiles.get(profile_id).cloned().ok_or_else(|| {
            SupervisorError::InvalidAuxiliary(format!(
                "headed browser is not registered for profile {profile_id}"
            ))
        })?;
        let display = self.auxiliary.poll(&pair.display_id)?;
        let browser = self.auxiliary.poll(&pair.browser_id)?;
        Ok(HeadedBrowserStatus {
            profile_id: profile_id.clone(),
            display,
            browser,
        })
    }

    /// Restarts only the exited members of a registered pair. Quarantined members remain fenced.
    ///
    /// # Errors
    /// Returns an error when the profile is unknown or a required restart is not permitted.
    pub fn restart_exited(
        &mut self,
        profile_id: &ProfileId,
    ) -> Result<HeadedBrowserStatus, SupervisorError> {
        let observed = self.poll(profile_id)?;
        let pair = self.profiles.get(profile_id).cloned().ok_or_else(|| {
            SupervisorError::InvalidAuxiliary(format!(
                "headed browser is not registered for profile {profile_id}"
            ))
        })?;
        let display = match observed.display.health {
            AuxiliaryProcessHealth::Running => observed.display,
            AuxiliaryProcessHealth::Exited => self.auxiliary.restart(&pair.display_id)?,
            AuxiliaryProcessHealth::Quarantined => {
                return Err(SupervisorError::AuxiliaryQuarantined(pair.display_id));
            }
        };
        let browser = match observed.browser.health {
            AuxiliaryProcessHealth::Running => observed.browser,
            AuxiliaryProcessHealth::Exited => self.auxiliary.restart(&pair.browser_id)?,
            AuxiliaryProcessHealth::Quarantined => {
                return Err(SupervisorError::AuxiliaryQuarantined(pair.browser_id));
            }
        };
        Ok(HeadedBrowserStatus {
            profile_id: profile_id.clone(),
            display,
            browser,
        })
    }

    /// Stops and forgets both members of one profile allocation.
    ///
    /// # Errors
    /// Returns an error when the profile is unknown or process control fails.
    pub fn stop(&mut self, profile_id: &ProfileId) -> Result<HeadedBrowserStatus, SupervisorError> {
        let pair = self.profiles.remove(profile_id).ok_or_else(|| {
            SupervisorError::InvalidAuxiliary(format!(
                "headed browser is not registered for profile {profile_id}"
            ))
        })?;
        let browser = self.auxiliary.stop(&pair.browser_id);
        let display = self.auxiliary.stop(&pair.display_id);
        Ok(HeadedBrowserStatus {
            profile_id: profile_id.clone(),
            display: display?,
            browser: browser?,
        })
    }

    pub fn status(&self, profile_id: &ProfileId) -> Option<HeadedBrowserStatus> {
        let pair = self.profiles.get(profile_id)?;
        Some(HeadedBrowserStatus {
            profile_id: profile_id.clone(),
            display: self.auxiliary.status(&pair.display_id)?,
            browser: self.auxiliary.status(&pair.browser_id)?,
        })
    }
}

fn validate_auxiliary_spec(spec: &AuxiliaryProcessSpec) -> Result<(), SupervisorError> {
    const ALLOWED_ENVIRONMENT: [&str; 7] = [
        "DISPLAY",
        "HOME",
        "LANG",
        "LC_ALL",
        "TMPDIR",
        "TZ",
        "XAUTHORITY",
    ];
    if !spec.executable.is_absolute()
        || !spec.runtime_directory.is_absolute()
        || spec.arguments.len() > 256
        || spec
            .arguments
            .iter()
            .any(|argument| argument.len() > 8_192 || argument.contains('\0'))
        || spec.environment.len() > ALLOWED_ENVIRONMENT.len()
        || spec.environment.iter().any(|(name, value)| {
            !ALLOWED_ENVIRONMENT.contains(&name.as_str())
                || value.len() > 8_192
                || value.contains('\0')
        })
        || spec.crash_limit == 0
        || spec.crash_window.is_zero()
    {
        return Err(SupervisorError::InvalidAuxiliary(
            "paths, arguments, environment, and crash bounds must be explicit and bounded".into(),
        ));
    }
    Ok(())
}

fn spawn_auxiliary(spec: &AuxiliaryProcessSpec) -> Result<Child, SupervisorError> {
    let mut command = Command::new(&spec.executable);
    command
        .args(&spec.arguments)
        .current_dir(&spec.runtime_directory)
        .env_clear()
        .envs(&spec.environment)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null());
    Ok(command.spawn()?)
}

fn auxiliary_status(process: &ManagedAuxiliaryProcess) -> AuxiliaryProcessStatus {
    AuxiliaryProcessStatus {
        id: process.spec.id.clone(),
        profile_id: process.spec.profile_id.clone(),
        kind: process.spec.kind,
        pid: process.child.as_ref().map(Child::id),
        health: if process.quarantined {
            AuxiliaryProcessHealth::Quarantined
        } else if process.child.is_some() {
            AuxiliaryProcessHealth::Running
        } else {
            AuxiliaryProcessHealth::Exited
        },
        crashes_in_window: process.crashes.len(),
    }
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
        let images = WorkerImageRegistry::open(state_dir.join("worker-images"), &executable)?;
        let control_directory =
            std::env::temp_dir().join(format!("keith-agent-control-{}", WorkerId::new()));
        Ok(Self {
            state_dir,
            control_directory,
            next_control_id: 1,
            lease_database,
            images,
            pinned_images: BTreeMap::new(),
            canary_mode: false,
            runtime_config,
            options,
            leases,
            workers: BTreeMap::new(),
            adopted_cleanup: BTreeMap::new(),
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
            if !process_is_alive(registration.pid) {
                continue;
            }
            let image = self.resolve_image(&registration.image_id)?;
            if registration.image_manifest_sha256 != image.manifest_sha256
                || registration.source_manifest_sha256 != image.source_manifest_sha256
            {
                return Err(SupervisorError::Image(ImageRegistryError::ArtifactMismatch));
            }
            let cleanup = AdoptedCleanupCandidate {
                process_start_identity: process_start_identity(registration.pid),
                registration: registration.clone(),
                executable: image.executable.clone(),
                executable_sha256: image.executable_sha256.clone(),
            };
            self.adopted_cleanup
                .insert(registration.root_tree_id.clone(), cleanup);
            let Some(grant) = self.leases.current(&registration.root_tree_id)? else {
                continue;
            };
            if grant.worker_id != registration.worker_id
                || grant.generation != registration.generation
            {
                continue;
            }
            let control = connect_control(&registration, &grant, self.options.startup_timeout)?;
            if !matches!(
                registration.state,
                WorkerRunState::Starting | WorkerRunState::Ready | WorkerRunState::Draining
            ) {
                continue;
            }
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
        let image = self.images.resolve_current()?;
        self.start_with_image(root_tree_id, image)
    }

    /// Registers an already verified immutable candidate and enables the credential-free canary
    /// worker entry point. The candidate identity is preserved in registration and generation
    /// state rather than being replaced with a bootstrap identity.
    ///
    /// # Errors
    /// Returns an error when the image bytes no longer match their installed digest.
    pub fn pin_canary_image(&mut self, image: InstalledImage) -> Result<(), SupervisorError> {
        verify_pinned_image(&image)?;
        self.canary_mode = true;
        self.pinned_images.insert(image.image_id.clone(), image);
        Ok(())
    }

    /// Starts a worker generation bound to one exact pinned candidate image.
    ///
    /// # Errors
    /// Returns an error when the image is absent, altered, or worker startup fails.
    pub fn start_pinned(
        &mut self,
        root_tree_id: RootTreeId,
        image_id: &str,
    ) -> Result<WorkerStatus, SupervisorError> {
        let image = self.resolve_image(image_id)?;
        self.start_with_image(root_tree_id, image)
    }

    fn start_with_image(
        &mut self,
        root_tree_id: RootTreeId,
        image: InstalledImage,
    ) -> Result<WorkerStatus, SupervisorError> {
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
        let mut child = match self.spawn(&grant, &control_socket, &image) {
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
                && registration.image_id == image.image_id
                && registration.image_manifest_sha256 == image.manifest_sha256
                && registration.source_manifest_sha256 == image.source_manifest_sha256
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

    fn resolve_image(&self, image_id: &str) -> Result<InstalledImage, SupervisorError> {
        if let Some(image) = self.pinned_images.get(image_id) {
            verify_pinned_image(image)?;
            return Ok(image.clone());
        }
        Ok(self.images.resolve(image_id)?)
    }

    fn spawn(
        &self,
        grant: &LeaseGrant,
        control_socket: &Path,
        image: &InstalledImage,
    ) -> Result<Child, SupervisorError> {
        let heartbeat_ms = self.options.heartbeat_interval.as_millis().max(1);
        let lease_ms = self.options.lease_duration.as_millis().max(1);
        let mut command = Command::new(&image.executable);
        if self.canary_mode {
            command.env_clear().arg("--canary");
            if let Some(path) = std::env::var_os("PATH") {
                command.env("PATH", path);
            }
        }
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
            .arg("--image-id")
            .arg(&image.image_id)
            .arg("--image-manifest-sha256")
            .arg(&image.manifest_sha256)
            .arg("--source-manifest-sha256")
            .arg(&image.source_manifest_sha256)
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
        let child = command
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .map_err(SupervisorError::from)?;
        Ok(child)
    }

    #[must_use]
    pub const fn image_registry(&self) -> &WorkerImageRegistry {
        &self.images
    }

    pub const fn image_registry_mut(&mut self) -> &mut WorkerImageRegistry {
        &mut self.images
    }

    /// Reclaims superseded images while protecting every live generation's bound image.
    ///
    /// # Errors
    /// Returns an error when registry persistence or filesystem reclamation fails.
    pub fn reclaim_images(
        &mut self,
        retained_history: usize,
    ) -> Result<Vec<String>, SupervisorError> {
        let live = self
            .workers
            .values()
            .map(|worker| worker.registration.image_id.clone())
            .collect();
        Ok(self.images.reclaim(retained_history, &live)?)
    }

    pub fn statuses(&self) -> Vec<WorkerStatus> {
        self.workers
            .values()
            .map(|worker| status_for(worker, self.options.stale_heartbeat))
            .collect()
    }

    /// Returns the exact roots currently owned by live supervisor generations.
    pub fn active_roots(&self) -> Vec<RootTreeId> {
        self.workers.keys().cloned().collect()
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
        self.execute_streaming(root_tree_id, generation, request, &mut |_| {})
    }

    /// Executes a request and forwards its ordered runtime events before returning the result.
    ///
    /// # Errors
    ///
    /// Returns an error for stale ownership, private transport loss, or response mismatch.
    pub fn execute_streaming(
        &mut self,
        root_tree_id: &RootTreeId,
        generation: Generation,
        request: RuntimeRequest,
        events: &mut dyn FnMut(RuntimeEvent),
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
                Ok(PrivateMessage::ExecutionEvent {
                    request_id: event_request_id,
                    event,
                }) if event_request_id == request_id => events(*event),
                Ok(
                    PrivateMessage::ExecutionResult { .. } | PrivateMessage::ExecutionEvent { .. },
                ) => {
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
                                | PrivateMessage::ExecutionEvent { .. }
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
        let candidates = std::mem::take(&mut self.adopted_cleanup);
        for candidate in candidates.into_values() {
            if !cleanup_identity_matches(&self.state_dir, &candidate)? {
                continue;
            }
            let pid = candidate.registration.pid;
            signal(pid, ProcessSignal::Graceful)?;
            let deadline = Instant::now() + self.options.drain_timeout;
            while cleanup_process_matches(&candidate) && Instant::now() < deadline {
                thread::sleep(Duration::from_millis(10));
            }
            if cleanup_process_matches(&candidate) {
                signal(pid, ProcessSignal::Force)?;
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

    /// Gracefully rolls one active root to one exact installed image.
    ///
    /// The current image is captured before drain. If candidate startup fails, the root is
    /// restarted from that exact image rather than from the registry's mutable current pointer.
    /// A successful return proves both a strictly newer generation and the requested healthy
    /// image identity.
    ///
    /// # Errors
    ///
    /// Returns an error when the root is inactive, the candidate is not installed or verified,
    /// drain/startup fails, or rollback cannot restore the pinned previous image.
    pub fn roll_to_image(
        &mut self,
        root_tree_id: &RootTreeId,
        candidate_image_id: &str,
    ) -> Result<WorkerRollProof, SupervisorError> {
        let previous = self
            .status(root_tree_id)
            .ok_or_else(|| SupervisorError::NotActive(root_tree_id.clone()))?;
        self.resolve_image(candidate_image_id)?;
        self.drain(root_tree_id)?;
        match self.start_pinned(root_tree_id.clone(), candidate_image_id) {
            Ok(candidate)
                if candidate.generation > previous.generation
                    && candidate.image_id == candidate_image_id
                    && candidate.health == WorkerHealth::Healthy =>
            {
                Ok(WorkerRollProof {
                    root_tree_id: root_tree_id.clone(),
                    previous_generation: previous.generation,
                    previous_image_id: previous.image_id,
                    generation: candidate.generation,
                    image_id: candidate.image_id,
                    health: candidate.health,
                })
            }
            Ok(candidate) => {
                let roll_error = format!(
                    "replacement proof mismatch: generation {:?}, image {}, health {:?}",
                    candidate.generation, candidate.image_id, candidate.health
                );
                let _ = self.drain(root_tree_id);
                self.rollback_after_failed_roll(
                    root_tree_id,
                    candidate_image_id,
                    previous,
                    roll_error,
                )
            }
            Err(error) => self.rollback_after_failed_roll(
                root_tree_id,
                candidate_image_id,
                previous,
                error.to_string(),
            ),
        }
    }

    /// Compatibility name for exact-image generation replacement used by promotion orchestration.
    pub fn restart_with_image(
        &mut self,
        root_tree_id: &RootTreeId,
        image_id: &str,
    ) -> Result<WorkerRollProof, SupervisorError> {
        self.roll_to_image(root_tree_id, image_id)
    }

    fn rollback_after_failed_roll(
        &mut self,
        root_tree_id: &RootTreeId,
        candidate_image_id: &str,
        previous: WorkerStatus,
        roll_error: String,
    ) -> Result<WorkerRollProof, SupervisorError> {
        match self.start_pinned(root_tree_id.clone(), &previous.image_id) {
            Ok(_) => Err(SupervisorError::Runtime(format!(
                "worker {root_tree_id} roll to image {candidate_image_id} failed and previous image {} was restored: {roll_error}",
                previous.image_id
            ))),
            Err(rollback_error) => Err(SupervisorError::RollbackFailed {
                root_tree_id: root_tree_id.clone(),
                candidate_image_id: candidate_image_id.into(),
                previous_image_id: previous.image_id,
                roll_error,
                rollback_error: rollback_error.to_string(),
            }),
        }
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

fn cleanup_identity_matches(
    state_dir: &Path,
    candidate: &AdoptedCleanupCandidate,
) -> Result<bool, SupervisorError> {
    let path = registration_path(state_dir, &candidate.registration.root_tree_id);
    let Ok(registration) = read_registration(&path) else {
        return Ok(false);
    };
    let executable_digest = fs::read(&candidate.executable)
        .ok()
        .map(|bytes| format!("{:x}", Sha256::digest(bytes)));
    Ok(registration.pid == candidate.registration.pid
        && registration.worker_id == candidate.registration.worker_id
        && registration.generation == candidate.registration.generation
        && registration.image_id == candidate.registration.image_id
        && registration.image_manifest_sha256 == candidate.registration.image_manifest_sha256
        && registration.source_manifest_sha256 == candidate.registration.source_manifest_sha256
        && executable_digest.as_deref() == Some(candidate.executable_sha256.as_str())
        && process_executable_matches(candidate)
        && cleanup_process_matches(candidate))
}

fn cleanup_process_matches(candidate: &AdoptedCleanupCandidate) -> bool {
    process_is_alive(candidate.registration.pid)
        && process_start_identity(candidate.registration.pid) == candidate.process_start_identity
}

#[cfg(unix)]
fn process_executable_matches(candidate: &AdoptedCleanupCandidate) -> bool {
    fs::read_link(format!("/proc/{}/exe", candidate.registration.pid))
        .ok()
        .and_then(|path| fs::canonicalize(path).ok())
        == fs::canonicalize(&candidate.executable).ok()
}

#[cfg(windows)]
fn process_executable_matches(_candidate: &AdoptedCleanupCandidate) -> bool {
    true
}

#[cfg(not(any(unix, windows)))]
fn process_executable_matches(_candidate: &AdoptedCleanupCandidate) -> bool {
    false
}

#[cfg(unix)]
fn process_start_identity(pid: u32) -> Option<String> {
    let stat = fs::read_to_string(format!("/proc/{pid}/stat")).ok()?;
    let (_, fields) = stat.rsplit_once(") ")?;
    fields.split_whitespace().nth(19).map(str::to_owned)
}

#[cfg(windows)]
fn process_start_identity(pid: u32) -> Option<String> {
    let output = Command::new("wmic")
        .args([
            "process",
            "where",
            &format!("ProcessId={pid}"),
            "get",
            "CreationDate",
            "/value",
        ])
        .output()
        .ok()?;
    String::from_utf8_lossy(&output.stdout)
        .lines()
        .find_map(|line| line.strip_prefix("CreationDate=").map(str::to_owned))
}

#[cfg(not(any(unix, windows)))]
fn process_start_identity(_pid: u32) -> Option<String> {
    None
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
        image_id: worker.registration.image_id.clone(),
        image_manifest_sha256: worker.registration.image_manifest_sha256.clone(),
        source_manifest_sha256: worker.registration.source_manifest_sha256.clone(),
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

fn verify_pinned_image(image: &InstalledImage) -> Result<(), SupervisorError> {
    let metadata = fs::symlink_metadata(&image.executable)?;
    if !image.verified || !metadata.is_file() || metadata.file_type().is_symlink() {
        return Err(SupervisorError::Image(ImageRegistryError::ArtifactMismatch));
    }
    let digest = Sha256::digest(fs::read(&image.executable)?).iter().fold(
        String::new(),
        |mut value, byte| {
            use std::fmt::Write as _;
            let _ = write!(value, "{byte:02x}");
            value
        },
    );
    if digest != image.executable_sha256
        || image.image_id != image.manifest_sha256
        || image.source_manifest_sha256.len() != 64
    {
        return Err(SupervisorError::Image(ImageRegistryError::ArtifactMismatch));
    }
    Ok(())
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
